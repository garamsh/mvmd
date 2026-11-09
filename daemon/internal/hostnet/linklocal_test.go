package hostnet_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func icmpv6Any() machine.NetworkRule {
	return machine.NetworkRule{Prefix: netip.MustParsePrefix("::/0"), Protocol: machine.ProtocolICMPv6}
}

// A neighbour solicitation is let through the tap only with hop limit 255. The
// kernel drops other hop limits itself when the host receives one, which would
// hide a missing match, so the solicitation is addressed to another Machine and
// forwarded: the host decrements its hop limit and delivers it, and only the
// tap's own check stands in the way.
func TestTap_AdmitsNeighbourDiscoveryOnlyWithHopLimit255(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		from := l.start("from", machine.NetworkPolicy{Egress: []machine.NetworkRule{icmpv6Any()}})
		to := l.start("to", machine.NetworkPolicy{Ingress: []machine.NetworkRule{icmpv6Any()}})

		solicit := func(hop byte) []byte {
			body := append(make([]byte, 4), to.ip6.AsSlice()...)
			return icmp6(from.hostMAC, from.mac, from.ip6, to.ip6, hop, typeNeighbourSolicit, body)
		}
		// The host decrements the hop limit as it forwards.
		arrivesWith := func(hop byte) func(seen) bool {
			return func(s seen) bool { return s.icmpType == typeNeighbourSolicit && s.hop == hop }
		}
		assert.False(t, outcome(t, from.port, to.port, solicit(254), solicit(255), arrivesWith(253), arrivesWith(254)),
			"a solicitation with hop limit 254 is dropped")
		to.port.drain()
		from.port.send(solicit(255))
		assert.True(t, to.port.receives(arrivesWith(254)), "control: the same solicitation with 255 passes")
	})
}

// The guest's IPv6 link-local address is the EUI-64 one its MAC gives (ADR 0011);
// no other link-local source passes the tap, and the EUI-64 one does.
func TestTap_AdmitsTheMachinesEUI64LinkLocalSourceAndNoOther(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", machine.NetworkPolicy{Egress: []machine.NetworkRule{
			{Prefix: netip.MustParsePrefix("fe80::/10"), Protocol: machine.ProtocolICMPv6},
		}})
		stranger := netip.MustParseAddr("fe80::dead:beef")
		// The host can answer either, so an answer to the stranger would show
		// its echo request passed.
		for _, a := range []netip.Addr{g.ll6, stranger} {
			require.NoError(t, netlink.NeighSet(&netlink.Neigh{
				LinkIndex: g.linkIndex, State: netlink.NUD_PERMANENT, IP: a.AsSlice(), HardwareAddr: g.mac,
			}))
		}
		const echoRequest, echoReply = 128, 129
		echo := func(from netip.Addr) []byte {
			return icmp6(g.hostMAC, g.mac, from, netip.MustParseAddr("fe80::1"), 64, echoRequest, []byte{0, 1, 0, 1})
		}
		replies := map[netip.Addr]bool{}
		g.port.drain()
		g.port.send(echo(stranger))
		g.port.send(echo(g.ll6))
		require.True(t, g.port.receives(func(s seen) bool {
			if s.icmpType == echoReply {
				replies[s.dst] = true
			}
			return replies[g.ll6]
		}), "control: the host answers an echo request from the Machine's EUI-64 address")
		assert.False(t, replies[stranger], "a link-local source that is not the Machine's EUI-64 address is dropped")
	})
}
