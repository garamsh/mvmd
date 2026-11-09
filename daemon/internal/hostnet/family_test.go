package hostnet_test

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
)

// A node that serves one family drops the other at its taps entirely, and a
// node that serves both passes both.
func TestFamilies_EachWorksAloneAndTogether(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cfg          hostnet.Config
		with4, with6 bool
	}{
		{"IPv4 alone", ipv4Only(), true, false},
		{"IPv6 alone", ipv6Only(), false, true},
		{"both", dualStack(), true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inNamespace(t, func(t *testing.T) {
				l := newLab(t, tc.cfg)
				g := l.start("m", egressTo(5000))
				assert.Equal(t, tc.with4, g.ip4.IsValid(), "the Machine holds an IPv4 address iff the node serves IPv4")
				assert.Equal(t, tc.with6, g.ip6.IsValid(), "the Machine holds an IPv6 address iff the node serves IPv6")

				// What the guest would send were it configured for the family
				// the node does not serve.
				as4 := netip.MustParseAddr("10.77.0.2")
				as6 := netip.MustParseAddr("fd77::2")
				if tc.with4 {
					as4 = g.ip4
					require.True(t, g.reaches(t, g.to4(outside4, 5000), outside4, 5000), "IPv4 passes")
				}
				if tc.with6 {
					as6 = g.ip6
					require.True(t, g.reaches(t, g.to6(outside6, 5000), outside6, 5000), "IPv6 passes")
				}
				switch {
				case tc.with4 && !tc.with6:
					frame := udp6(g.hostMAC, g.mac, as6, outside6, 40000, 5000)
					assert.False(t, outcomeVia(t, g, frame, g.to4(outside4, control), outside6, 5000), "IPv6 is dropped")
				case tc.with6 && !tc.with4:
					frame := udp4(g.hostMAC, g.mac, as4, outside4, 40000, 5000)
					assert.False(t, outcomeVia(t, g, frame, g.to6(outside6, control), outside4, 5000), "IPv4 is dropped")
				}
			})
		})
	}
}

// outcomeVia is reaches with the control frame given, for a family the Machine
// has no address in.
func outcomeVia(t *testing.T, g *guest, frame, ctl []byte, dst netip.Addr, dport uint16) bool {
	t.Helper()
	return outcome(t, g.port, g.l.outside, frame, ctl, udpTo(dst, dport), func(s seen) bool { return s.dport == control && s.proto == 17 })
}
