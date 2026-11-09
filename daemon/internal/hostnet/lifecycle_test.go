package hostnet_test

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// linkWatch records the link changes the kernel announces, in the order it
// sends them, and what the nftables tables held when each tap came up.
type linkWatch struct {
	mu sync.Mutex
	// up are the devices seen with their link up, and rulesAtUp the chains of
	// the adapter's tables at that moment.
	up        map[string]bool
	rulesAtUp map[string][]string
	seen      map[string]bool
}

func watchLinks(t *testing.T) *linkWatch {
	t.Helper()
	w := &linkWatch{up: map[string]bool{}, rulesAtUp: map[string][]string{}, seen: map[string]bool{}}
	updates := make(chan netlink.LinkUpdate, 64)
	done := make(chan struct{})
	require.NoError(t, netlink.LinkSubscribe(updates, done))
	t.Cleanup(func() { close(done) })
	go func() {
		for u := range updates {
			name := u.Attrs().Name
			w.mu.Lock()
			w.seen[name] = true
			if u.IfInfomsg.Flags&unix.IFF_UP != 0 && !w.up[name] {
				w.up[name] = true
				w.rulesAtUp[name] = allChains()
			}
			w.mu.Unlock()
		}
	}()
	return w
}

// allChains names every chain of the adapter's tables, without failing a test
// from another goroutine.
func allChains() []string {
	c, err := nftables.New()
	if err != nil {
		return nil
	}
	chains, err := c.ListChains()
	if err != nil {
		return nil
	}
	var names []string
	for _, ch := range chains {
		if ch.Table.Name == "mvmd" {
			names = append(names, ch.Name)
		}
	}
	return names
}

// settle waits until the watcher has seen a change made after everything the
// test did before, so what it holds is complete.
func (w *linkWatch) settle(t *testing.T) {
	t.Helper()
	marker := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "mvmdsettle"}}
	require.NoError(t, netlink.LinkAdd(marker))
	defer func() { _ = netlink.LinkDel(marker) }()
	require.True(t, eventually(1*time.Second, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return w.seen["mvmdsettle"]
	}), "the watcher saw nothing of the marker device")
}

func (w *linkWatch) forget(name string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.up, name)
	delete(w.rulesAtUp, name)
}

func (w *linkWatch) wasUp(name string) (bool, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.up[name], w.rulesAtUp[name]
}

func TestApply_TheLinkComesUpOnlyAfterTheRulesAreCommitted(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		w := watchLinks(t)
		g := l.start("m", machine.NetworkPolicy{})
		w.settle(t)
		up, chainsThen := w.wasUp(g.tap)
		require.True(t, up, "the link came up")
		assert.Contains(t, chainsThen, g.tap, "the tap's chain was committed when the link came up")
		assert.Contains(t, chainsThen, "eg_"+g.tap)
	})
}

// leftovers lists what a Machine's tap would still hold in the kernel: its
// device, its chains and the rules of the base chains that jump to them.
func leftovers(t *testing.T, tap string) []string {
	t.Helper()
	var found []string
	if linkNamed(t, tap) != nil {
		found = append(found, "device "+tap)
	}
	for _, name := range allChains() {
		if strings.Contains(name, tap) {
			found = append(found, "chain "+name)
		}
	}
	c, err := nftables.New()
	require.NoError(t, err)
	inet := &nftables.Table{Family: nftables.TableFamilyINet, Name: "mvmd"}
	for _, base := range []string{"forward", "input", "output"} {
		rules, err := c.GetRules(inet, &nftables.Chain{Table: inet, Name: base})
		if err != nil {
			continue
		}
		for _, r := range rules {
			if string(r.UserData) == tap {
				found = append(found, "rule in "+base)
			}
		}
	}
	return found
}

// tapOf is the name of the tap of a Machine, found by making it and removing it.
func tapOf(t *testing.T, l *lab, id machine.MachineID) string {
	t.Helper()
	g := l.start(id, machine.NetworkPolicy{})
	require.NoError(t, l.net.Release(context.Background(), id))
	require.Empty(t, leftovers(t, g.tap), "setup: Release removed it")
	return g.tap
}

func TestApply_AFailureAtAnyStepLeavesNoTapAndNoRules(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		w := watchLinks(t)
		ctx := context.Background()
		// A Machine that stays, so the table exists and its rules can be shown
		// to survive a neighbour's failure.
		stays := l.start("stays", egressTo(5000))

		try := func(id machine.MachineID, policy machine.NetworkPolicy) error {
			_, err := l.net.Allocate(ctx, id, "")
			require.NoError(t, err)
			_, err = l.net.Apply(ctx, id, vmmUser, policy)
			return err
		}
		survives := func(t *testing.T) {
			assert.True(t, stays.reaches(t, stays.to4(outside4, 5000), outside4, 5000), "the Machine that stayed is still filtered and still passes what it allows")
		}

		t.Run("the commit fails", func(t *testing.T) {
			tap := tapOf(t, l, "commit")
			w.settle(t)
			w.forget(tap) // making it for its name brought it up once
			// A base chain where a regular one is wanted: the kernel refuses the
			// transaction.
			c, err := nftables.New()
			require.NoError(t, err)
			inet := &nftables.Table{Family: nftables.TableFamilyINet, Name: "mvmd"}
			c.AddChain(&nftables.Chain{Table: inet, Name: "eg_" + tap, Type: nftables.ChainTypeFilter,
				Hooknum: nftables.ChainHookForward, Priority: nftables.ChainPriorityFilter})
			require.NoError(t, c.Flush())
			require.Contains(t, allChains(), "eg_"+tap, "setup: the conflicting chain exists")

			err = try("commit", machine.NetworkPolicy{})
			require.ErrorContains(t, err, "commit the rules", "it failed at the commit")
			assert.Empty(t, leftovers(t, tap))
			w.settle(t)
			up, _ := w.wasUp(tap)
			assert.False(t, up, "the link was never up, since the rules were not committed")
			survives(t)
		})
		t.Run("the route is refused", func(t *testing.T) {
			tap := tapOf(t, l, "route")
			identity, err := l.net.Allocate(ctx, "route", "")
			require.NoError(t, err)
			// The address is already routed elsewhere, so routing it to the tap fails
			// after the link is up.
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: "dum0"}}
			require.NoError(t, netlink.LinkAdd(dummy))
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })
			require.NoError(t, netlink.LinkSetUp(dummy))
			v4 := identity.Addresses[0]
			require.True(t, v4.Is4())
			require.NoError(t, netlink.RouteAdd(&netlink.Route{
				LinkIndex: dummy.Attrs().Index, Scope: netlink.SCOPE_LINK,
				Dst: &net.IPNet{IP: v4.AsSlice(), Mask: net.CIDRMask(32, 32)},
			}))

			_, err = l.net.Apply(ctx, "route", vmmUser, machine.NetworkPolicy{})
			require.Error(t, err)
			assert.Empty(t, leftovers(t, tap))
			survives(t)
		})
		t.Run("the device name is taken by something that is not a tap", func(t *testing.T) {
			tap := tapOf(t, l, "taken")
			dummy := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: tap}}
			require.NoError(t, netlink.LinkAdd(dummy))
			t.Cleanup(func() { _ = netlink.LinkDel(dummy) })

			require.Error(t, try("taken", machine.NetworkPolicy{}))
			assert.NotNil(t, linkNamed(t, tap), "the device is not the adapter's, and stays")
			assert.Empty(t, slices.DeleteFunc(leftovers(t, tap), func(s string) bool { return s == "device "+tap }), "no rules were left")
		})
		t.Run("a policy that cannot be expressed", func(t *testing.T) {
			bad := machine.NetworkPolicy{Egress: []machine.NetworkRule{allow("10.0.0.0/8", machine.ProtocolICMPv6)}}
			tap := tapOf(t, l, "badpolicy")
			require.Error(t, try("badpolicy", bad))
			assert.Empty(t, leftovers(t, tap))
			good := machine.NetworkPolicy{Egress: []machine.NetworkRule{allow("10.0.0.0/8", machine.ProtocolICMP)}}
			_, err := l.net.Apply(ctx, "badpolicy", vmmUser, good)
			assert.NoError(t, err, "control: the same Machine with an expressible policy is applied")
		})
	})
}

func TestWithdrawAndRelease_RemoveTheMachinesRulesAndTap(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		ctx := context.Background()
		gone := l.start("gone", egressTo(5000))
		stays := l.start("stays", egressTo(5000))
		require.Empty(t, leftovers(t, "none"))
		require.NotEmpty(t, leftovers(t, gone.tap), "setup: the Machine has a tap and rules")

		require.NoError(t, l.net.Withdraw(ctx, "gone"))
		assert.Empty(t, leftovers(t, gone.tap))
		assert.NotEmpty(t, leftovers(t, stays.tap), "control: another Machine's tap and rules stay")
		assert.True(t, stays.reaches(t, stays.to4(outside4, 5000), outside4, 5000), "control: and it still passes what it allows")
		require.NoError(t, l.net.Withdraw(ctx, "gone"), "withdrawing a Machine with no tap succeeds")

		// Withdraw keeps the addresses, so the Machine is applied again with the same ones.
		again, err := l.net.Allocate(ctx, "gone", "")
		require.NoError(t, err)
		assert.Equal(t, gone.identity, again)
		other, err := l.net.Allocate(ctx, "other", "")
		require.NoError(t, err)
		assert.NotContains(t, other.Addresses, gone.ip4, "control: while it holds them, no one else gets them")

		// Release frees them.
		require.NoError(t, l.net.Release(ctx, "gone"))
		require.NoError(t, l.net.Release(ctx, "other"))
		third, err := l.net.Allocate(ctx, "third", "")
		require.NoError(t, err)
		assert.Equal(t, gone.identity.Addresses, third.Addresses, "the addresses it held are free again")
		require.NoError(t, l.net.Release(ctx, "never-existed"), "releasing a Machine that holds nothing succeeds")
	})
}

func TestApply_AddressesAndRoutesFollowADR0011(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", machine.NetworkPolicy{})
		link := linkNamed(t, g.tap)

		v4, err := netlink.AddrList(link, netlink.FAMILY_V4)
		require.NoError(t, err)
		require.Len(t, v4, 1)
		assert.Equal(t, "10.77.0.1/32", v4[0].IPNet.String(), "the IPv4 gateway")
		v6, err := netlink.AddrList(link, netlink.FAMILY_V6)
		require.NoError(t, err)
		require.Len(t, v6, 1, "addr_gen_mode 1: fe80::1 is the host's only IPv6 address on the tap")
		assert.Equal(t, "fe80::1", v6[0].IP.String())

		routed := func(family int, dst netip.Addr) bool {
			routes, err := netlink.RouteListFiltered(family, &netlink.Route{LinkIndex: link.Attrs().Index}, netlink.RT_FILTER_OIF)
			require.NoError(t, err)
			return slices.ContainsFunc(routes, func(r netlink.Route) bool {
				a, _ := netip.AddrFromSlice(r.Dst.IP)
				ones, bits := r.Dst.Mask.Size()
				return r.Dst != nil && a == dst && ones == bits
			})
		}
		assert.True(t, routed(netlink.FAMILY_V4, g.ip4), "the Machine's IPv4 address is routed to its tap")
		assert.True(t, routed(netlink.FAMILY_V6, g.ip6), "the Machine's IPv6 address is routed to its tap")
	})
}

func TestOutput_DropsIPFromTheSocketsOfVMMUsers(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		// The host may send to the guest's port 8080 from its gateway address.
		g := l.start("m", machine.NetworkPolicy{Ingress: []machine.NetworkRule{allow("10.77.0.0/24", machine.ProtocolUDP, 8080, control)}})

		// send makes a UDP socket as the user, in a thread of its own that
		// ends with it, so no other test runs as that user.
		send := func(uid int, dport uint16) error {
			done := make(chan error, 1)
			go func() {
				runtime.LockOSThread() // never unlocked: the thread ends with the goroutine
				done <- func() error {
					// Setup failures do not wrap their cause, so an EPERM below is
					// the packet's and not the setup's.
					if err := unix.Setfsuid(uid); err != nil {
						return fmt.Errorf("setup: %v", err)
					}
					fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
					if err != nil {
						return fmt.Errorf("setup: %v", err)
					}
					defer unix.Close(fd)
					return unix.Sendto(fd, []byte("mvmd"), 0, &unix.SockaddrInet4{Port: int(dport), Addr: g.ip4.As4()})
				}()
			}()
			return <-done
		}
		const inRange, outOfRange = 4500, 3000
		g.port.drain()
		// The kernel reports a packet that netfilter dropped on output as EPERM.
		require.ErrorIs(t, send(inRange, 8080), unix.EPERM)
		require.NoError(t, send(outOfRange, 9999))
		saw8080 := false
		require.True(t, g.port.receives(func(s seen) bool {
			saw8080 = saw8080 || (s.proto == 17 && s.dport == 8080)
			return s.proto == 17 && s.dport == 9999
		}), "control: a socket of a user outside the VMM range sends")
		assert.False(t, saw8080, "a socket of a user in the VMM range sends nothing")
	})
}
