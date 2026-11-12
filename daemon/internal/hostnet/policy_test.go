package hostnet_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestPolicy_EgressPassesWhatARuleAllowsAndDropsTheRest(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", egressTo(5000))
		wide := l.start("wide", machine.NetworkPolicy{Egress: []machine.NetworkRule{
			allow("198.51.100.0/24", machine.ProtocolUDP, 5000, control),
			allow("2001:db8:2::/64", machine.ProtocolUDP, 5000, control),
		}})
		wide.ctl4, wide.ctl6 = other4, other6

		t.Run("IPv4", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to4(outside4, 5000), outside4, 5000), "control: an allowed destination and port")
			assert.False(t, g.reaches(t, g.to4(outside4, 5001), outside4, 5001), "a port no rule names")
			assert.False(t, g.reaches(t, g.to4(other4, 5000), other4, 5000), "a destination no rule names")
			assert.True(t, wide.reaches(t, wide.to4(other4, 5000), other4, 5000), "control: that destination is routable, and a rule that names it passes it")
		})
		t.Run("IPv6", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to6(outside6, 5000), outside6, 5000), "control: an allowed destination and port")
			assert.False(t, g.reaches(t, g.to6(outside6, 5001), outside6, 5001), "a port no rule names")
			assert.False(t, g.reaches(t, g.to6(other6, 5000), other6, 5000), "a destination no rule names")
			assert.True(t, wide.reaches(t, wide.to6(other6, 5000), other6, 5000), "control: that destination is routable, and a rule that names it passes it")
		})
		t.Run("what leaves the node is masqueraded", func(t *testing.T) {
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
			assert.Equal(t, hostUp4, sourceOf(outside4, g.to4(outside4, 5000)))
			assert.Equal(t, hostUp6, sourceOf(outside6, g.to6(outside6, 5000)))
		})
	})
}

func TestPolicy_ADestinationInsideANoMasqueradePrefixKeepsTheMachinesSource(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		cfg := dualStack()
		cfg.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8:1::/64")}
		l := newLab(t, cfg)
		g := l.start("m", machine.NetworkPolicy{Egress: []machine.NetworkRule{
			allow("192.0.2.0/24", machine.ProtocolUDP, 5000),
			allow("198.51.100.0/24", machine.ProtocolUDP, 5000),
			allow("2001:db8:1::/64", machine.ProtocolUDP, 5000),
			allow("2001:db8:2::/64", machine.ProtocolUDP, 5000),
		}})
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
		require.NotEqual(t, hostUp4, g.ip4, "setup: the Machine's address is not the node's")
		assert.Equal(t, g.ip4, sourceOf(outside4, g.to4(outside4, 5000)), "IPv4 inside the prefix keeps the Machine's address")
		assert.Equal(t, hostUp4, sourceOf(other4, g.to4(other4, 5000)), "control: IPv4 outside it is masqueraded")
		assert.Equal(t, g.ip6, sourceOf(outside6, g.to6(outside6, 5000)), "IPv6 inside the prefix keeps the Machine's address")
		assert.Equal(t, hostUp6, sourceOf(other6, g.to6(other6, 5000)), "control: IPv6 outside it is masqueraded")
	})
}

func TestPolicy_IngressPassesWhatARuleAllowsAndDropsTheRest(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", machine.NetworkPolicy{Ingress: []machine.NetworkRule{
			allow("192.0.2.0/24", machine.ProtocolUDP, 8080, control),
			allow("2001:db8:1::/64", machine.ProtocolUDP, 8080, control),
		}})
		fromOutside4 := func(src netip.Addr, dport uint16) []byte {
			return udp4(l.upMAC, l.outsideMAC, src, g.ip4, 40000, dport)
		}
		fromOutside6 := func(src netip.Addr, dport uint16) []byte {
			return udp6(l.upMAC, l.outsideMAC, src, g.ip6, 40000, dport)
		}
		reaches := func(frame []byte, dst netip.Addr, dport uint16, ctl []byte) bool {
			return outcome(t, l.outside, g.port, frame, ctl, udpTo(dst, dport), func(s seen) bool { return s.dport == control && s.proto == 17 })
		}
		t.Run("IPv4", func(t *testing.T) {
			ctl := fromOutside4(outside4, control)
			assert.True(t, reaches(fromOutside4(outside4, 8080), g.ip4, 8080, ctl), "control: an allowed source and port")
			assert.False(t, reaches(fromOutside4(outside4, 8081), g.ip4, 8081, ctl), "a port no rule names")
			assert.False(t, reaches(fromOutside4(other4, 8080), g.ip4, 8080, ctl), "a source no rule names")
		})
		t.Run("IPv6", func(t *testing.T) {
			ctl := fromOutside6(outside6, control)
			assert.True(t, reaches(fromOutside6(outside6, 8080), g.ip6, 8080, ctl), "control: an allowed source and port")
			assert.False(t, reaches(fromOutside6(outside6, 8081), g.ip6, 8081, ctl), "a port no rule names")
			assert.False(t, reaches(fromOutside6(other6, 8080), g.ip6, 8080, ctl), "a source no rule names")
		})
	})
}

func TestPolicy_RepliesOfAnAcceptedConnectionPassAndNothingElseDoes(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, ipv4Only())
		g := l.start("m", egressTo(5000))

		// The guest opens a connection; what leaves is masqueraded to a port.
		l.outside.drain()
		g.port.send(g.to4(outside4, 5000))
		var natPort uint16
		require.True(t, l.outside.receives(func(s seen) bool {
			natPort = s.sport
			return s.proto == 17 && s.dport == 5000
		}))
		require.Equal(t, uint16(40000), natPort, "setup: the connection keeps the guest's source port, which the reply names")

		// The reply goes to the node's address and is translated back to the
		// guest's, whose source port is 40000. A packet sent straight to the
		// guest from the same peer is no reply, and the policy has no ingress
		// rule for it. The two arrive at different ports of the guest.
		reply := udp4(l.upMAC, l.outsideMAC, outside4, hostUp4, 5000, natPort)
		stranger := udp4(l.upMAC, l.outsideMAC, outside4, g.ip4, 5000, 40001)
		toGuest := func(dport uint16) func(seen) bool {
			return func(s seen) bool { return s.proto == 17 && s.dst == g.ip4 && s.dport == dport }
		}
		assert.False(t, outcome(t, l.outside, g.port, stranger, reply, toGuest(40001), toGuest(40000)),
			"a packet that is not part of the connection")
		l.outside.drain()
		l.outside.send(reply)
		assert.True(t, g.port.receives(toGuest(40000)), "control: the reply of the connection the guest opened")
	})
}
