package app_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/nstest"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// These tests wire the real Network adapter through the composition site, so
// they change the kernel's network configuration and run in a network
// namespace made for them (internal/nstest).

func networkConfigured(t *testing.T) app.Config {
	cfg := testConfig(t)
	cfg.Network = &app.NetworkConfig{IPv4Prefix: "10.78.0.0/24", IPv4Gateway: "10.78.0.1", IPv6Prefix: "fd78::/64"}
	return cfg
}

// tapChains names the chains the adapter's netdev table holds, one per tap, or
// nothing if the table is absent.
func tapChains(t *testing.T) []string {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	chains, err := c.ListChains()
	require.NoError(t, err)
	var names []string
	for _, ch := range chains {
		if ch.Table.Name == "mvmd" && ch.Table.Family == nftables.TableFamilyNetdev {
			names = append(names, ch.Name)
		}
	}
	return names
}

func hasLink(t *testing.T, name string) bool {
	t.Helper()
	_, err := netlink.LinkByName(name)
	if _, missing := err.(netlink.LinkNotFoundError); missing {
		return false
	}
	require.NoError(t, err)
	return true
}

// damage leaves the node as a crash or another tool would: mvmd's tables gone,
// and a tap that no Machine owns.
func damage(t *testing.T) {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyNetdev, Name: "mvmd"})
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "mvmd"})
	require.NoError(t, c.Flush())
	stale := &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "mvmd0123456789a"}, Mode: netlink.TUNTAP_MODE_TAP, Flags: netlink.TUNTAP_NO_PI}
	require.NoError(t, netlink.LinkAdd(stale))
	t.Cleanup(func() { _ = netlink.LinkDel(stale) })
	require.Empty(t, tapChains(t), "setup: the table is gone")
}

func TestNetwork_AStartMakesATapAndAStopRemovesIt(t *testing.T) {
	nstest.Run(t, func(t *testing.T) {
		ctx := context.Background()
		n := start(t, networkConfigured(t))
		require.Nil(t, n.app.Substitutes.Network, "the configuration replaced the in-memory Network")
		populate(t, n)
		require.Len(t, tapChains(t), 1, "Start of \"a\" made its tap and committed its rules")
		assert.True(t, hasLink(t, tapChains(t)[0]))

		_, err := n.client.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "a", GracePeriod: durationpb.New(time.Second)})
		require.NoError(t, err)
		assert.Empty(t, tapChains(t), "Stop removed the rules")
	})
}

// A daemon that starts over a stale or missing table replaces it from the
// records before it serves, and its taps with it.
func TestNetwork_RestartReplacesTheTableFromTheRecordsBeforeServing(t *testing.T) {
	nstest.Run(t, func(t *testing.T) {
		cfg := networkConfigured(t)
		first := start(t, cfg)
		populate(t, first)
		tap := tapChains(t)
		require.Len(t, tap, 1, "setup: \"a\" is Running with its tap")
		first.shutdown(t)
		damage(t)

		a, err := app.New(cfg)
		require.NoError(t, err)
		superviseA(a) // "a"'s supervisor survived, so "a" is still Running
		reached, release := make(chan struct{}), make(chan struct{})
		app.HoldBeforeReconcile(a, func() { close(reached); <-release })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- a.Serve(ctx) }()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("the daemon did not stop")
			}
		})
		<-reached

		assert.Empty(t, tapChains(t), "control: until the daemon reconciles the table is still missing")
		assert.True(t, hasLink(t, "mvmd0123456789a"), "control: and so is the stale tap")
		_, statErr := os.Lstat(cfg.Socket.Path)
		require.ErrorIs(t, statErr, os.ErrNotExist, "nothing is served meanwhile")

		close(release)
		select {
		case <-a.Ready():
		case err := <-done:
			t.Fatalf("the daemon ended: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("the daemon was not ready")
		}
		assert.Equal(t, tap, tapChains(t), "the table holds the Running Machine's chain again")
		assert.False(t, hasLink(t, "mvmd0123456789a"), "and the tap that belongs to no Machine is gone")
		assert.True(t, hasLink(t, tap[0]), "the Running Machine's tap stays")
	})
}

// A Running Machine whose tap is gone cannot be vouched for and is Faulted; the
// daemon serves, and the other Machine's network is restored.
func TestNetwork_RestartFaultsAMachineThatLostItsTapAndServesTheRest(t *testing.T) {
	for _, lose := range []bool{false, true} {
		name := "every tap present"
		if lose {
			name = "one tap deleted"
		}
		t.Run(name, func(t *testing.T) {
			nstest.Run(t, func(t *testing.T) {
				ctx := context.Background()
				cfg := networkConfigured(t)
				first := start(t, cfg)
				populate(t, first)
				_, err := first.client.CreateMachine(ctx, create("c"))
				require.NoError(t, err)
				_, err = first.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "c"})
				require.NoError(t, err)
				taps := tapChains(t)
				require.Len(t, taps, 2, "setup: \"a\" and \"c\" are Running with a tap each")
				first.shutdown(t)
				if lose {
					link, err := netlink.LinkByName(taps[0])
					require.NoError(t, err)
					require.NoError(t, netlink.LinkDel(link))
				}

				second := startWith(t, cfg, func(a *app.App) {
					superviseA(a)
					require.NoError(t, a.Substitutes.Supervisors.Start(ctx, "c", machine.ProcessDescription{}, machine.User{UID: 1001, GID: 1001}, 0))
				})
				var faulted, running []string
				for _, id := range []string{"a", "c"} {
					m, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: id})
					require.NoError(t, err)
					switch m.GetMachine().GetState() {
					case v1alpha1.MachineState_MACHINE_STATE_FAULTED:
						faulted = append(faulted, id)
						assert.Contains(t, m.GetMachine().GetLastFailureReason(), "network could not be restored")
					case v1alpha1.MachineState_MACHINE_STATE_RUNNING:
						running = append(running, id)
					}
				}
				if !lose {
					assert.Empty(t, faulted, "control: with every tap present both stay Running")
					assert.Len(t, tapChains(t), 2)
					return
				}
				assert.Len(t, faulted, 1, "the Machine whose tap is gone is Faulted")
				assert.Len(t, running, 1, "and the other stays Running")
				assert.Len(t, tapChains(t), 1, "with its rules restored")
			})
		})
	}
}
