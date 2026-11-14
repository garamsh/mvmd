package hostnet_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Whichever of two Machines was made first, a packet from one to the other
// passes the sender's egress and then the receiver's ingress (ADR 0012).
func TestPolicy_ASendersEgressDecidesWhetherItWasMadeBeforeOrAfterTheReceiver(t *testing.T) {
	const peers = "10.77.0.0/24"
	receiverPolicy := machine.NetworkPolicy{Ingress: []machine.NetworkRule{
		allow(peers, machine.ProtocolUDP, 8080, control),
	}}
	// The denying egress still allows the control port, so that the control
	// frame shows what the dropped one did not reach.
	policyOf := func(egressAllows bool) machine.NetworkPolicy {
		if egressAllows {
			return machine.NetworkPolicy{Egress: []machine.NetworkRule{allow(peers, machine.ProtocolUDP, 8080, control)}}
		}
		return machine.NetworkPolicy{Egress: []machine.NetworkRule{allow(peers, machine.ProtocolUDP, control)}}
	}
	for _, order := range []struct {
		name          string
		senderIsLater bool
	}{{"the receiver was made first", true}, {"the sender was made first", false}} {
		for _, egressAllows := range []bool{true, false} {
			name := order.name + ", and the sender's egress denies the port"
			if egressAllows {
				name = order.name + ", and the sender's egress allows the port"
			}
			t.Run(name, func(t *testing.T) {
				inNamespace(t, func(t *testing.T) {
					l := newLab(t, ipv4Only())
					senderPolicy := policyOf(egressAllows)
					var sender, receiver *guest
					if order.senderIsLater {
						receiver = l.start("receiver", receiverPolicy)
						sender = l.start("sender", senderPolicy)
					} else {
						sender = l.start("sender", senderPolicy)
						receiver = l.start("receiver", receiverPolicy)
					}
					reaches := func() bool {
						return outcome(t, sender.port, receiver.port,
							sender.to4(receiver.ip4, 8080),
							sender.to4(receiver.ip4, control),
							udpTo(receiver.ip4, 8080),
							func(s seen) bool { return s.dport == control && s.proto == 17 })
					}
					assert.Equal(t, egressAllows, reaches(), "the packet passes only if the sender's egress allows it")

					// The records are replayed receiver first, whichever order
					// the Machines were made in.
					dropTables(t)
					n := restarted(t, l)
					lost, err := n.Reconcile(context.Background(), []machine.Machine{
						record(receiver, machine.StateRunning, receiverPolicy),
						record(sender, machine.StateRunning, senderPolicy),
					})
					require.NoError(t, err)
					require.Empty(t, lost)
					assert.Equal(t, egressAllows, reaches(), "after a restart")
				})
			})
		}
	}
}
