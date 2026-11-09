package memory

import (
	"context"
	"fmt"
	"net/netip"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Network is an in-memory machine.Network. Its steps are "allocate", "apply",
// "withdraw" and "release".
type Network struct {
	Failures

	mu        sync.Mutex
	next      int
	allocated map[machine.MachineID]machine.NetworkIdentity
	applied   map[machine.MachineID]machine.NetworkPolicy
}

// NewNetwork returns a Network that has assigned nothing.
func NewNetwork() *Network {
	return &Network{
		allocated: make(map[machine.MachineID]machine.NetworkIdentity),
		applied:   make(map[machine.MachineID]machine.NetworkPolicy),
	}
}

// Allocate implements machine.Network.
func (n *Network) Allocate(_ context.Context, id machine.MachineID, mac string) (machine.NetworkIdentity, error) {
	if err := n.Enter("allocate"); err != nil {
		return machine.NetworkIdentity{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if have, ok := n.allocated[id]; ok {
		return have, nil
	}
	n.next++
	if mac == "" {
		mac = fmt.Sprintf("02:00:00:00:00:%02x", n.next)
	}
	identity := machine.NetworkIdentity{
		MAC:       mac,
		Addresses: []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 0, byte(n.next)})},
		Gateways:  []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 255, 254})},
		Resolvers: []netip.Addr{netip.AddrFrom4([4]byte{10, 0, 255, 253})},
	}
	n.allocated[id] = identity
	return identity, nil
}

// Apply implements machine.Network.
func (n *Network) Apply(_ context.Context, id machine.MachineID, _ machine.User, policy machine.NetworkPolicy) (string, error) {
	if err := n.Enter("apply"); err != nil {
		return "", err
	}
	n.mu.Lock()
	n.applied[id] = policy
	n.mu.Unlock()
	return "tap-" + string(id), nil
}

// Withdraw implements machine.Network.
func (n *Network) Withdraw(_ context.Context, id machine.MachineID) error {
	if err := n.Enter("withdraw"); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.applied, id)
	n.mu.Unlock()
	return nil
}

// Release implements machine.Network.
func (n *Network) Release(_ context.Context, id machine.MachineID) error {
	if err := n.Enter("release"); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.allocated, id)
	delete(n.applied, id)
	n.mu.Unlock()
	return nil
}

// Allocated reports whether the Machine holds addresses.
func (n *Network) Allocated(id machine.MachineID) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	_, ok := n.allocated[id]
	return ok
}

// Applied returns the policy committed at the Machine's tap, if a tap exists.
func (n *Network) Applied(id machine.MachineID) (machine.NetworkPolicy, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	p, ok := n.applied[id]
	return p, ok
}
