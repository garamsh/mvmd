package hostnet_test

import (
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// outcome sends frame and then control from sender, and reports whether
// observer saw a frame matching target before it saw the control. The control
// takes the same path as the frame, so once it has arrived a frame that has not
// was dropped; if it never arrives the test fails, since nothing is shown.
func outcome(t *testing.T, sender, observer *port, frame, control []byte, target, isControl func(seen) bool) bool {
	t.Helper()
	observer.drain()
	sender.send(frame)
	sender.send(control)
	got := false
	deadline := time.Now().Add(3 * time.Second)
	for {
		f, ok := observer.next(time.Until(deadline))
		require.True(t, ok, "the control frame never arrived, so the outcome shows nothing")
		s, ok := parse(f)
		if !ok {
			continue
		}
		if isControl(s) {
			return got
		}
		if target(s) {
			got = true
		}
	}
}

func udpTo(dst netip.Addr, dport uint16) func(seen) bool {
	return func(s seen) bool { return s.proto == 17 && s.dst == dst && s.dport == dport }
}

const control = 9999

// policy allows the guest to send UDP to the first outside on the ports, and
// the control port.
func egressTo(ports ...uint16) machine.NetworkPolicy {
	ports = append(ports, control)
	return machine.NetworkPolicy{Egress: []machine.NetworkRule{
		allow("192.0.2.0/24", machine.ProtocolUDP, ports...),
		allow("2001:db8:1::/64", machine.ProtocolUDP, ports...),
	}}
}

func (g *guest) to4(dst netip.Addr, dport uint16) []byte {
	return udp4(g.hostMAC, g.mac, g.ip4, dst, 40000, dport)
}

func (g *guest) to6(dst netip.Addr, dport uint16) []byte {
	return udp6(g.hostMAC, g.mac, g.ip6, dst, 40000, dport)
}

// reaches reports whether the guest's frame comes out of the uplink, with the
// control frame of its family sent after it.
func (g *guest) reaches(t *testing.T, frame []byte, dst netip.Addr, dport uint16) bool {
	t.Helper()
	ctl := g.to4(g.ctl4, control)
	if dst.Is6() {
		ctl = g.to6(g.ctl6, control)
	}
	return outcome(t, g.port, g.l.outside, frame, ctl, udpTo(dst, dport), func(s seen) bool { return s.dport == control && s.proto == 17 })
}

func TestTap_DropsAFrameThatIsNotTheMachinesOwn(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", egressTo(5000))
		otherMAC := net.HardwareAddr{0x02, 0xaa, 0xbb, 0xcc, 0xdd, 0xee}

		t.Run("IPv4", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to4(outside4, 5000), outside4, 5000), "control: its own frame passes")
			spoofed := netip.MustParseAddr("10.77.0.99")
			assert.False(t, g.reaches(t, udp4(g.hostMAC, g.mac, spoofed, outside4, 40000, 5000), outside4, 5000), "another source address")
			assert.False(t, g.reaches(t, udp4(g.hostMAC, otherMAC, g.ip4, outside4, 40000, 5000), outside4, 5000), "another MAC")
		})
		t.Run("IPv6", func(t *testing.T) {
			assert.True(t, g.reaches(t, g.to6(outside6, 5000), outside6, 5000), "control: its own frame passes")
			spoofed := netip.MustParseAddr("fd77::dead")
			assert.False(t, g.reaches(t, udp6(g.hostMAC, g.mac, spoofed, outside6, 40000, 5000), outside6, 5000), "another source address")
			assert.False(t, g.reaches(t, udp6(g.hostMAC, otherMAC, g.ip6, outside6, 40000, 5000), outside6, 5000), "another MAC")
		})
		t.Run("ARP", func(t *testing.T) {
			// The host answers any request for its gateway address, so an
			// answer addressed to a sender shows its request passed the tap.
			ask := func(ether, sha net.HardwareAddr, spa string) []byte {
				return arpFrame(net.HardwareAddr{0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, ether, 1, sha, netip.MustParseAddr(spa), make(net.HardwareAddr, 6), netip.MustParseAddr("10.77.0.1"))
			}
			answered := map[netip.Addr]bool{}
			g.port.drain()
			g.port.send(ask(g.mac, g.mac, "10.77.0.77"))    // another sender address
			g.port.send(ask(g.mac, otherMAC, "10.77.0.78")) // another sender MAC in the message
			g.port.send(ask(g.mac, g.mac, g.ip4.String()))  // its own
			require.True(t, g.port.receives(func(s seen) bool {
				if s.arpOp == 2 {
					answered[s.dst] = true
				}
				return answered[g.ip4]
			}), "control: the host answers the Machine's own request")
			for _, a := range []string{"10.77.0.77", "10.77.0.78"} {
				assert.False(t, answered[netip.MustParseAddr(a)], "a request with %s is not answered", a)
			}
		})
	})
}
