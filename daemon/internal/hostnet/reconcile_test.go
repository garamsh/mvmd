package hostnet_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// record is the inside's record of a Machine the test made.
func record(g *guest, state machine.State, policy machine.NetworkPolicy) machine.Machine {
	return machine.Machine{ID: g.id, State: state, Spec: machine.Spec{Policy: policy}, User: vmmUser, Network: g.identity}
}

// restarted is an adapter that knows nothing, as after mvmd's restart.
func restarted(t *testing.T, l *lab) *hostnet.Network {
	t.Helper()
	n, err := hostnet.New(l.cfg)
	require.NoError(t, err)
	return n
}

func dropTables(t *testing.T) {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyNetdev, Name: "mvmd"})
	c.DelTable(&nftables.Table{Family: nftables.TableFamilyINet, Name: "mvmd"})
	require.NoError(t, c.Flush())
	require.Empty(t, allChains(), "setup: the tables are gone")
}

func staleTap(t *testing.T, name string) {
	t.Helper()
	tap := &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: name}, Mode: netlink.TUNTAP_MODE_TAP, Flags: netlink.TUNTAP_NO_PI}
	require.NoError(t, netlink.LinkAdd(tap))
	t.Cleanup(func() { _ = netlink.LinkDel(tap) })
}

func TestReconcile_ReplacesTheTableFromTheRecordsAndRemovesTheStale(t *testing.T) {
	policy := egressTo(5000)
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T)
	}{
		{"the table is missing", dropTables},
		{"the table holds a chain no Machine owns", func(t *testing.T) {
			c, err := nftables.New()
			require.NoError(t, err)
			c.AddChain(&nftables.Chain{Table: &nftables.Table{Family: nftables.TableFamilyINet, Name: "mvmd"}, Name: "eg_mvmdghost"})
			require.NoError(t, c.Flush())
			require.Contains(t, allChains(), "eg_mvmdghost", "setup: the stray chain exists")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inNamespace(t, func(t *testing.T) {
				l := newLab(t, dualStack())
				running := l.start("running", policy)
				stopped := l.start("stopped", policy)
				tc.damage(t)
				staleTap(t, "mvmd0123456789a")
				// Devices that are the operator's, with names that start as
				// the adapter's do, are not the adapter's to delete.
				staleTap(t, "mvmd0")
				staleTap(t, "mvmdABCDEF01234")

				n := restarted(t, l)
				lost, err := n.Reconcile(context.Background(), []machine.Machine{
					record(running, machine.StateRunning, policy),
					record(stopped, machine.StateStopped, policy),
				})
				require.NoError(t, err)
				assert.Empty(t, lost)
				assert.NotNil(t, linkNamed(t, "mvmd0"), "a tap named mvmd0 is not the adapter's")
				assert.NotNil(t, linkNamed(t, "mvmdABCDEF01234"), "nor is one whose name is not in the adapter's form")

				netdev, inet := chainsOf(t)
				assert.ElementsMatch(t, []string{running.tap}, netdev, "one tap chain, the Running Machine's")
				assert.ElementsMatch(t, []string{"forward", "input", "output", "postrouting", "eg_" + running.tap, "in_" + running.tap}, inet)
				assert.NotNil(t, linkNamed(t, running.tap), "the Running Machine's tap stays")
				assert.Nil(t, linkNamed(t, stopped.tap), "a Stopped Machine's tap is stale")
				assert.Nil(t, linkNamed(t, "mvmd0123456789a"), "so is one that belongs to no Machine")

				// The Machine is filtered by the new table as it was by the old.
				assert.True(t, running.reaches(t, running.to4(outside4, 5000), outside4, 5000), "control: what the policy allows passes")
				assert.False(t, running.reaches(t, running.to4(outside4, 5001), outside4, 5001), "what it does not is dropped")

				// What the records hold is held again: no address is given twice.
				fresh, err := n.Allocate(context.Background(), "fresh", "")
				require.NoError(t, err)
				for _, a := range fresh.Addresses {
					assert.NotContains(t, running.identity.Addresses, a)
					assert.NotContains(t, stopped.identity.Addresses, a, "a Stopped Machine keeps its addresses")
				}
			})
		})
	}
}

func TestReconcile_KeepsWhatMayStillHaveAVMMAndReportsWhatIsLost(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		policy := egressTo(5000)
		paused := l.start("paused", policy)
		faulted := l.start("faulted", policy)
		created := l.start("created", policy)
		gone, err := l.net.Allocate(context.Background(), "gone", "")
		require.NoError(t, err)
		goneRecord := machine.Machine{ID: "gone", State: machine.StateRunning, Spec: machine.Spec{Policy: policy}, User: vmmUser, Network: gone}
		noTap, err := l.net.Allocate(context.Background(), "faulted-no-tap", "")
		require.NoError(t, err)

		n := restarted(t, l)
		lost, err := n.Reconcile(context.Background(), []machine.Machine{
			record(paused, machine.StatePaused, policy),
			record(faulted, machine.StateFaulted, policy),
			record(created, machine.StateCreated, policy),
			goneRecord,
			{ID: "faulted-no-tap", State: machine.StateFaulted, Spec: machine.Spec{Policy: policy}, User: vmmUser, Network: noTap},
		})
		require.NoError(t, err)
		assert.Equal(t, []machine.MachineID{"gone"}, lost, "a Running Machine whose tap is gone is reported, and a Faulted one with no tap, which is nothing to protect, is not")

		assert.NotNil(t, linkNamed(t, paused.tap), "a Paused Machine is kept")
		assert.NotNil(t, linkNamed(t, faulted.tap), "a Faulted Machine whose tap exists is kept, since its VMM may run")
		assert.Nil(t, linkNamed(t, created.tap), "a Created Machine has no VMM")
		netdev, _ := chainsOf(t)
		assert.True(t, slices.Equal(slices.Sorted(slices.Values(netdev)), slices.Sorted(slices.Values([]string{paused.tap, faulted.tap}))),
			"the table is replaced all the same, with the Machines that have a tap")
	})
}
