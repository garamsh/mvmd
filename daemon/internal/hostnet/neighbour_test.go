package hostnet_test

import (
	"net"
	"net/netip"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// defaultRouterVia reports whether the host holds an IPv6 default route out of
// the device through the router.
func defaultRouterVia(t *testing.T, ifindex int, router netip.Addr) bool {
	t.Helper()
	routes, err := netlink.RouteListFiltered(netlink.FAMILY_V6, &netlink.Route{LinkIndex: ifindex}, netlink.RT_FILTER_OIF)
	require.NoError(t, err)
	for _, r := range routes {
		if gw, ok := netip.AddrFromSlice(r.Gw); ok && gw == router {
			return true
		}
	}
	return false
}

func TestGuest_ARouterAdvertisementIsDropped(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		// The policy allows every ICMPv6 packet to every address, the host's
		// among them, so that nothing but the tap's own check stops the
		// advertisement: the inet chains would drop it otherwise.
		g := l.start("m", machine.NetworkPolicy{Egress: []machine.NetworkRule{allow("::/0", machine.ProtocolICMPv6)}})

		// A bare tap, with nothing to filter it, is the control: it shows that
		// the host would act on this advertisement, here by taking a default
		// route through its sender. Taps that forward ignore advertisements
		// unless accept_ra is 2, so both are set to 2.
		bare := &netlink.Tuntap{LinkAttrs: netlink.LinkAttrs{Name: "bare0"}, Mode: netlink.TUNTAP_MODE_TAP, Flags: netlink.TUNTAP_NO_PI}
		require.NoError(t, netlink.LinkAdd(bare))
		t.Cleanup(func() { _ = netlink.LinkDel(bare) })
		bareLink, err := netlink.LinkByName("bare0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkSetUp(bareLink))
		bareGuest := tapPort(t, "bare0")
		for _, dev := range []string{"bare0", g.tap} {
			require.NoError(t, os.WriteFile("/proc/sys/net/ipv6/conf/"+dev+"/accept_ra", []byte("2"), 0o644))
		}

		bareMAC := net.HardwareAddr{0x02, 0, 0, 0, 0, 0x42}
		bareLL := netip.MustParseAddr("fe80::42")
		bareGuest.send(routerAdvert(bareMAC, bareLL))
		require.True(t, eventually(func() bool { return defaultRouterVia(t, bareLink.Attrs().Index, bareLL) }),
			"control: the host takes a default route from an advertisement that nothing filters")

		g.port.send(routerAdvert(g.mac, g.ll6))
		assert.False(t, defaultRouterVia(t, g.linkIndex, g.ll6), "an advertisement from the Machine's tap is dropped")
	})
}

func TestGuest_NeighbourDiscoveryWithHopLimit255Passes(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", machine.NetworkPolicy{Ingress: []machine.NetworkRule{
			allow("2001:db8:1::/64", machine.ProtocolUDP, 8080),
		}})
		// The host must ask for the guest's address now.
		require.NoError(t, netlink.NeighDel(&netlink.Neigh{LinkIndex: g.linkIndex, IP: g.ip6.AsSlice()}))

		hostLL := netip.MustParseAddr("fe80::1")

		t.Run("a solicitation for the gateway is answered", func(t *testing.T) {
			stranger := netip.MustParseAddr("fe80::dead:beef")
			got := map[netip.Addr]bool{}
			g.port.drain()
			g.port.send(neighbourSolicit(g.mac, stranger, hostLL, 255))
			g.port.send(neighbourSolicit(g.mac, g.ll6, hostLL, 255))
			require.True(t, g.port.receives(func(s seen) bool {
				if s.icmpType == typeNeighbourAdvert && s.src == hostLL && s.hop == 255 {
					got[s.dst] = true
				}
				return got[g.ll6]
			}), "control: the host answers the Machine's own solicitation")
			assert.False(t, got[stranger], "a solicitation from another source address is dropped at the tap")
		})

		t.Run("an advertisement from the Machine resolves it for the host", func(t *testing.T) {
			// Traffic for the guest makes the host solicit it. The guest's
			// advertisement is what lets the packet through.
			g.port.drain()
			l.outside.send(udp6(l.upMAC, l.outsideMAC, outside6, g.ip6, 40000, 8080))
			var asker netip.Addr
			require.True(t, g.port.receives(func(s seen) bool {
				asker = s.src
				return s.icmpType == typeNeighbourSolicit && s.hop == 255
			}), "the host solicits the guest, which the output chain lets through")
			g.port.send(neighbourAdvert(g.hostMAC, g.mac, g.ip6, asker, g.ip6, 255))
			assert.True(t, g.port.receives(udpTo(g.ip6, 8080)), "the packet waiting for the answer is delivered")
		})
	})
}
