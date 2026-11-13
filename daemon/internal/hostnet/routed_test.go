package hostnet_test

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The routed prefixes of the tests (ADR 0061): neither is the node's, and
// neither is reachable from the outside except through the Machine.
const (
	routed4 = "203.0.113.0/28"
	routed6 = "2001:db8:100::/56"
)

var (
	inRouted4  = netip.MustParseAddr("203.0.113.5")
	inRouted6  = netip.MustParseAddr("2001:db8:100::5")
	notRouted4 = netip.MustParseAddr("203.0.113.99")
	notRouted6 = netip.MustParseAddr("2001:db8:101::5")
)

func TestTap_AdmitsASourceInARoutedPrefixAndDropsEveryOtherSource(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.startRouted("m", egressTo(5000), routed4, routed6)
		otherMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}

		t.Run("IPv4", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to4(outside4, 5000), outside4, 5000), "control: its own address passes")
			assert.True(t, g.reaches(t, udp4(g.hostMAC, g.mac, inRouted4, outside4, 40000, 5000), outside4, 5000), "a source in the routed prefix")
			assert.False(t, g.reaches(t, udp4(g.hostMAC, g.mac, notRouted4, outside4, 40000, 5000), outside4, 5000), "a source next to the routed prefix")
			assert.False(t, g.reaches(t, udp4(g.hostMAC, otherMAC, inRouted4, outside4, 40000, 5000), outside4, 5000), "a routed source from another MAC")
		})
		t.Run("IPv6", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to6(outside6, 5000), outside6, 5000), "control: its own address passes")
			assert.True(t, g.reaches(t, udp6(g.hostMAC, g.mac, inRouted6, outside6, 40000, 5000), outside6, 5000), "a source in the routed prefix")
			assert.False(t, g.reaches(t, udp6(g.hostMAC, g.mac, notRouted6, outside6, 40000, 5000), outside6, 5000), "a source next to the routed prefix")
			assert.False(t, g.reaches(t, udp6(g.hostMAC, otherMAC, inRouted6, outside6, 40000, 5000), outside6, 5000), "a routed source from another MAC")
		})
	})
}

func TestTap_DropsARoutedSourceOfAnotherMachine(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		owner := l.startRouted("owner", egressTo(5000), routed4, routed6)
		plain := l.start("plain", egressTo(5000))

		assert.True(t, plain.reaches(t, plain.to4(outside4, 5000), outside4, 5000), "control: its own address passes")
		assert.False(t, plain.reaches(t, udp4(plain.hostMAC, plain.mac, inRouted4, outside4, 40000, 5000), outside4, 5000),
			"another Machine's routed prefix is not its source to send")
		assert.False(t, plain.reaches(t, udp6(plain.hostMAC, plain.mac, inRouted6, outside6, 40000, 5000), outside6, 5000), "nor in IPv6")
		assert.True(t, owner.reaches(t, udp4(owner.hostMAC, owner.mac, inRouted4, outside4, 40000, 5000), outside4, 5000), "control: the owner may")
	})
}

func TestRouted_TrafficToAnAddressInTheRoutedPrefixReachesTheTapWhenThePolicyAllows(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.startRouted("m", machine.NetworkPolicy{Ingress: []machine.NetworkRule{
			allow("192.0.2.0/24", machine.ProtocolUDP, 8080, control),
			allow("2001:db8:1::/64", machine.ProtocolUDP, 8080, control),
		}}, routed4, routed6)
		g.reachable(inRouted4)
		g.reachable(inRouted6)
		reaches := func(frame, ctl []byte, dst netip.Addr, dport uint16) bool {
			return outcome(t, l.outside, g.port, frame, ctl, udpTo(dst, dport), func(s seen) bool { return s.dport == control && s.proto == 17 })
		}
		t.Run("IPv4", func(t *testing.T) {
			ctl := udp4(l.upMAC, l.outsideMAC, outside4, inRouted4, 40000, control)
			assert.True(t, reaches(udp4(l.upMAC, l.outsideMAC, outside4, inRouted4, 40000, 8080), ctl, inRouted4, 8080), "an allowed source and port")
			assert.False(t, reaches(udp4(l.upMAC, l.outsideMAC, outside4, inRouted4, 40000, 8081), ctl, inRouted4, 8081), "a port no rule names")
			assert.False(t, reaches(udp4(l.upMAC, l.outsideMAC, other4, inRouted4, 40000, 8080), ctl, inRouted4, 8080), "a source no rule names")
		})
		t.Run("IPv6", func(t *testing.T) {
			ctl := udp6(l.upMAC, l.outsideMAC, outside6, inRouted6, 40000, control)
			assert.True(t, reaches(udp6(l.upMAC, l.outsideMAC, outside6, inRouted6, 40000, 8080), ctl, inRouted6, 8080), "an allowed source and port")
			assert.False(t, reaches(udp6(l.upMAC, l.outsideMAC, outside6, inRouted6, 40000, 8081), ctl, inRouted6, 8081), "a port no rule names")
			assert.False(t, reaches(udp6(l.upMAC, l.outsideMAC, other6, inRouted6, 40000, 8080), ctl, inRouted6, 8080), "a source no rule names")
		})
	})
}

func TestRouted_ANotherMachinesTrafficToTheRoutedPrefixObeysTheBothPolicies(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		// The senders are made before the owner. The base chains hold the
		// Machines' jumps in the order they were made, and a receiver's ingress
		// accept that comes first ends the chain before a later sender's egress is
		// looked at; that order is not what this test is about.
		//
		// The peer may send to the routed prefix on both ports, so that it is
		// the owner's ingress that stops 8081; the shut peer may send nowhere.
		peer := l.start("peer", machine.NetworkPolicy{Egress: []machine.NetworkRule{
			allow(routed4, machine.ProtocolUDP, 8080, 8081, control),
		}})
		shut := l.start("shut", machine.NetworkPolicy{})
		owner := l.startRouted("owner", machine.NetworkPolicy{Ingress: []machine.NetworkRule{
			allow("10.77.0.0/24", machine.ProtocolUDP, 8080, control),
		}}, routed4)
		owner.reachable(inRouted4)

		reaches := func(from *guest, dport uint16) bool {
			frame := udp4(from.hostMAC, from.mac, from.ip4, inRouted4, 40000, dport)
			ctl := udp4(from.hostMAC, from.mac, from.ip4, inRouted4, 40000, control)
			return outcome(t, from.port, owner.port, frame, ctl, udpTo(inRouted4, dport), func(s seen) bool { return s.dport == control && s.proto == 17 })
		}
		assert.True(t, reaches(peer, 8080), "control: both policies allow it")
		assert.False(t, reaches(peer, 8081), "the owner's ingress does not name the port")

		// What the owner receives still names the peer, as it does for the
		// owner's address: a routed prefix is not translated on the way.
		var from netip.Addr
		owner.port.drain()
		peer.port.send(udp4(peer.hostMAC, peer.mac, peer.ip4, inRouted4, 40000, 8080))
		require.True(t, owner.port.receives(func(s seen) bool {
			from = s.src
			return s.proto == 17 && s.dport == 8080
		}))
		assert.Equal(t, peer.ip4, from)

		// The shut peer has no egress rule, so nothing it sends passes, and the
		// owner's control frame must still arrive for the outcome to show it.
		frame := udp4(shut.hostMAC, shut.mac, shut.ip4, inRouted4, 40000, 8080)
		ctl := udp4(peer.hostMAC, peer.mac, peer.ip4, inRouted4, 40000, control)
		owner.port.drain()
		shut.port.send(frame)
		peer.port.send(ctl)
		got := false
		for {
			f, ok := owner.port.next(2 * time.Second)
			require.True(t, ok, "the control never arrived")
			s, ok := parse(f)
			if !ok {
				continue
			}
			if s.proto == 17 && s.dport == control {
				break
			}
			if s.src == shut.ip4 {
				got = true
			}
		}
		assert.False(t, got, "a Machine whose egress allows nothing cannot reach the routed prefix")
	})
}

func TestRouted_MasqueradeAndNoMasqueradeWorkAsForTheMachinesAddress(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		cfg := dualStack()
		cfg.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:1::/64")}
		l := newLab(t, cfg)
		g := l.startRouted("m", machine.NetworkPolicy{Egress: []machine.NetworkRule{
			allow("192.0.2.0/24", machine.ProtocolUDP, 5000),
			allow("198.51.100.0/24", machine.ProtocolUDP, 5000),
			allow("2001:db8:1::/64", machine.ProtocolUDP, 5000),
			allow("2001:db8:2::/64", machine.ProtocolUDP, 5000),
		}}, routed4, routed6)
		sourceOf := func(dst netip.Addr, frame []byte) netip.Addr {
			l.outside.drain()
			g.port.send(frame)
			var got netip.Addr
			require.True(t, l.outside.receives(func(s seen) bool {
				if s.proto == 17 && s.dst == dst && s.dport == 5000 {
					got = s.src
					return true
				}
				return false
			}))
			return got
		}
		from4 := func(dst netip.Addr) []byte { return udp4(g.hostMAC, g.mac, inRouted4, dst, 40000, 5000) }
		from6 := func(dst netip.Addr) []byte { return udp6(g.hostMAC, g.mac, inRouted6, dst, 40000, 5000) }

		assert.Equal(t, inRouted4, sourceOf(outside4, from4(outside4)), "IPv4 inside a no-masquerade prefix keeps the routed source")
		assert.Equal(t, hostUp4, sourceOf(other4, from4(other4)), "IPv4 outside it is masqueraded")
		assert.Equal(t, inRouted6, sourceOf(outside6, from6(outside6)), "IPv6 inside a no-masquerade prefix keeps the routed source")
		assert.Equal(t, hostUp6, sourceOf(other6, from6(other6)), "IPv6 outside it is masqueraded")
	})
}

func TestRouted_TheRoutesAndTheRulesAreMadeAgainFromTheRecordAfterARestart(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		policy := egressTo(5000)
		g := l.startRouted("m", policy, routed4, routed6)
		require.NotEmpty(t, g.identity.RoutedPrefixes, "setup: the identity holds the prefixes")
		routes := func() []string {
			var out []string
			for _, family := range []int{netlink.FAMILY_V4, netlink.FAMILY_V6} {
				link, err := netlink.LinkByIndex(g.linkIndex)
				require.NoError(t, err)
				rs, err := netlink.RouteList(link, family)
				require.NoError(t, err)
				for _, r := range rs {
					if r.Dst != nil {
						out = append(out, r.Dst.String())
					}
				}
			}
			return out
		}
		require.Contains(t, routes(), routed4, "setup: the route exists")
		require.Contains(t, routes(), routed6, "setup: the route exists")

		// A route that was removed and tables that are gone.
		for _, p := range g.routed {
			require.NoError(t, netlink.RouteDel(&netlink.Route{LinkIndex: g.linkIndex, Dst: ipNet(p), Scope: scopeOf(p)}))
		}
		require.NotContains(t, routes(), routed4, "setup: the route is gone")
		dropTables(t)

		n := restarted(t, l)
		lost, err := n.Reconcile(context.Background(), []machine.Machine{record(g, machine.StateRunning, policy)})
		require.NoError(t, err)
		require.Empty(t, lost)

		assert.Contains(t, routes(), routed4, "the IPv4 route is made again")
		assert.Contains(t, routes(), routed6, "the IPv6 route is made again")
		assert.True(t, g.reaches(t, udp4(g.hostMAC, g.mac, inRouted4, outside4, 40000, 5000), outside4, 5000), "the tap admits a routed source again")
		assert.False(t, g.reaches(t, udp4(g.hostMAC, g.mac, notRouted4, outside4, 40000, 5000), outside4, 5000), "control: and no other")

		// What the record holds is held again: the prefix is not given twice.
		_, err = n.Allocate(context.Background(), "late", "", netip.MustParsePrefix("203.0.113.16/29"))
		assert.NoError(t, err, "control: a prefix next to the held one is free")
		_, err = n.Allocate(context.Background(), "later", "", netip.MustParsePrefix("203.0.113.0/29"))
		require.Error(t, err)
		assert.ErrorIs(t, err, machine.ErrConflict, "a prefix inside the held one is not")
	})
}

func ipNet(p netip.Prefix) *net.IPNet {
	return &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}
}

func scopeOf(p netip.Prefix) netlink.Scope {
	if p.Addr().Is4() {
		return netlink.SCOPE_LINK
	}
	return netlink.SCOPE_UNIVERSE
}

func TestApplyThenWithdraw_RemovesTheRoutedPrefixRoutes(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.startRouted("m", egressTo(5000), routed4)
		has := func() bool {
			rs, err := netlink.RouteList(nil, netlink.FAMILY_V4)
			require.NoError(t, err)
			for _, r := range rs {
				if r.Dst != nil && r.Dst.String() == routed4 {
					return true
				}
			}
			return false
		}
		require.True(t, has(), "setup: the route exists while the tap does")
		require.NoError(t, l.net.Withdraw(context.Background(), g.id))
		assert.False(t, has(), "withdrawing the tap removes the route with it")
		_, err := l.net.Apply(context.Background(), g.id, vmmUser, egressTo(5000))
		require.NoError(t, err)
		assert.True(t, has(), "applying again routes the prefix again, from what Allocate held")
	})
}

func TestAllocate_RefusesARoutedPrefixTheNodeCannotGive(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		held := netip.MustParsePrefix("203.0.113.0/28")
		for _, tc := range []struct {
			name   string
			cfg    func() hostnet.Config
			prefix string
			kind   error
			// naming is what the refusal must mention.
			naming string
		}{
			{"a family the node does not serve", func() hostnet.Config { return ipv4Only() }, "2001:db8:100::/56", machine.ErrInvalid, "2001:db8:100::/56"},
			{"the node's IPv4 Machine prefix", func() hostnet.Config { return dualStack() }, "10.77.0.0/24", machine.ErrConflict, "10.77.0.0/24"},
			{"a prefix that contains the node's", func() hostnet.Config { return dualStack() }, "10.77.0.0/16", machine.ErrConflict, "10.77.0.0/16"},
			{"the node's IPv6 Machine prefix", func() hostnet.Config { return dualStack() }, "fd77::/64", machine.ErrConflict, "fd77::/64"},
			{"another Machine's routed prefix", func() hostnet.Config { return dualStack() }, "203.0.113.0/29", machine.ErrConflict, "203.0.113.0/29"},
			{"a prefix that contains another Machine's routed prefix", func() hostnet.Config { return dualStack() }, "203.0.0.0/16", machine.ErrConflict, "203.0.0.0/16"},
			{"a prefix that contains a resolver", func() hostnet.Config {
				c := dualStack()
				c.Resolvers = []netip.Addr{netip.MustParseAddr("198.51.100.53")}
				return c
			}, "198.51.100.0/24", machine.ErrConflict, "198.51.100.0/24"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				l := newLab(t, tc.cfg())
				_, err := l.net.Allocate(context.Background(), "holder", "", held)
				require.NoError(t, err, "setup: another Machine holds a routed prefix")
				before, err := l.net.Allocate(context.Background(), "probe", "")
				require.NoError(t, err)
				require.NoError(t, l.net.Release(context.Background(), "probe"))

				_, err = l.net.Allocate(context.Background(), "m", "", netip.MustParsePrefix(tc.prefix))
				require.Error(t, err)
				assert.ErrorIs(t, err, tc.kind)
				assert.Contains(t, err.Error(), tc.naming, "the refusal names the prefix")

				// Nothing was assigned: the Machine's addresses are the first free.
				after, err := l.net.Allocate(context.Background(), "after", "")
				require.NoError(t, err)
				assert.Equal(t, before.Addresses, after.Addresses, "a refused request holds nothing")
			})
		}

		t.Run("control: a prefix the node can give", func(t *testing.T) {
			l := newLab(t, dualStack())
			got, err := l.net.Allocate(context.Background(), "m", "", held, netip.MustParsePrefix("2001:db8:100::/56"))
			require.NoError(t, err)
			assert.Equal(t, []netip.Prefix{held, netip.MustParsePrefix("2001:db8:100::/56")}, got.RoutedPrefixes)
		})
	})
}
