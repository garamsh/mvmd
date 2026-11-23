package memory

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Network is an in-memory machine.Network. Its steps are "allocate", "apply",
// "withdraw" and "release".
type Network struct {
	Failures

	mu         sync.Mutex
	next       int
	allocated  map[machine.MachineID]machine.NetworkIdentity
	applied    map[machine.MachineID]machine.NetworkPolicy
	lose       map[machine.MachineID]bool
	reconciled []machine.MachineID
	// limit is how many Machines may hold addresses; zero is no limit.
	limit int
}

// NewNetwork returns a Network that has assigned nothing.
func NewNetwork() *Network {
	return &Network{
		allocated: make(map[machine.MachineID]machine.NetworkIdentity),
		applied:   make(map[machine.MachineID]machine.NetworkPolicy),
		lose:      make(map[machine.MachineID]bool),
	}
}

// The memory Network serves one IPv4 prefix, from which it numbers Machines in
// the order it sees them, with a gateway and a resolver at its end.
var (
	memoryPrefix   = netip.MustParsePrefix("10.0.0.0/16")
	memoryGateway  = netip.AddrFrom4([4]byte{10, 0, 255, 254})
	memoryResolver = netip.AddrFrom4([4]byte{10, 0, 255, 253})
)

// checkRequested refuses a requested address as the real adapter does (ADR
// 0069): outside the prefix, in a family it does not serve, or the network or
// broadcast address is machine.ErrInvalid, and the gateway, the resolver or an
// address another Machine holds is machine.ErrConflict. Callers hold n.mu.
func (n *Network) checkRequested(a netip.Addr) error {
	switch {
	case !a.Is4():
		return machine.NewError(machine.ErrInvalid, "requested address %s is in a family the node does not serve", a)
	case !memoryPrefix.Contains(a):
		return machine.NewError(machine.ErrInvalid, "requested address %s is outside the node's Machine prefix %s", a, memoryPrefix)
	case a == memoryPrefix.Addr():
		return machine.NewError(machine.ErrInvalid, "requested address %s is the network address of %s", a, memoryPrefix)
	case !memoryPrefix.Contains(a.Next()):
		return machine.NewError(machine.ErrInvalid, "requested address %s is the broadcast address of %s", a, memoryPrefix)
	case a == memoryGateway:
		return machine.NewError(machine.ErrConflict, "requested address %s is the gateway", a)
	case a == memoryResolver:
		return machine.NewError(machine.ErrConflict, "requested address %s is a resolver", a)
	}
	for other, held := range n.allocated {
		if slices.Contains(held.Addresses, a) {
			return machine.NewError(machine.ErrConflict, "requested address %s is held by Machine %q", a, other)
		}
	}
	return nil
}

// unheld is the next address in the numbering that no Machine holds, which a
// requested address may have taken. Callers hold n.mu.
func (n *Network) unheld() netip.Addr {
	for {
		a := netip.AddrFrom4([4]byte{10, 0, byte(n.next >> 8), byte(n.next)})
		held := false
		for _, other := range n.allocated {
			held = held || slices.Contains(other.Addresses, a)
		}
		if !held {
			return a
		}
		n.next++
	}
}

// Allocate implements machine.Network.
func (n *Network) Allocate(_ context.Context, id machine.MachineID, want machine.Want, routed ...netip.Prefix) (machine.NetworkIdentity, error) {
	if err := n.Enter("allocate"); err != nil {
		return machine.NetworkIdentity{}, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if have, ok := n.allocated[id]; ok {
		return have, nil
	}
	for _, p := range routed {
		for other, held := range n.allocated {
			for _, q := range held.RoutedPrefixes {
				if p.Overlaps(q) {
					return machine.NetworkIdentity{}, machine.NewError(machine.ErrConflict, "routed prefix %s overlaps routed prefix %s of Machine %q", p, q, other)
				}
			}
		}
	}
	if n.limit > 0 && len(n.allocated) >= n.limit {
		return machine.NetworkIdentity{}, fmt.Errorf("%w: all %d of the Network's addresses are held", machine.ErrNoFreeAddress, n.limit)
	}
	address := netip.Addr{}
	for _, a := range want.Addresses {
		if err := n.checkRequested(a); err != nil {
			return machine.NetworkIdentity{}, err
		}
		address = a
	}
	n.next++
	mac := want.MAC
	if mac == "" {
		mac = fmt.Sprintf("02:00:00:00:00:%02x", n.next)
	}
	if !address.IsValid() {
		address = n.unheld()
	}
	identity := machine.NetworkIdentity{
		MAC:       mac,
		Addresses: []netip.Addr{address},
		Gateways:  []netip.Addr{memoryGateway},
		Resolvers: []netip.Addr{memoryResolver},
		// Held as given, so the identity echoes the request as the real adapter's does.
		RoutedPrefixes: append([]netip.Prefix(nil), routed...),
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
func (n *Network) Withdraw(ctx context.Context, id machine.MachineID) error {
	if err := n.Enter("withdraw"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.applied, id)
	n.mu.Unlock()
	return nil
}

// Release implements machine.Network.
func (n *Network) Release(ctx context.Context, id machine.MachineID) error {
	if err := n.Enter("release"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	n.mu.Lock()
	delete(n.allocated, id)
	delete(n.applied, id)
	n.mu.Unlock()
	return nil
}

// Reconcile implements machine.Network. It takes the identities the Machines
// hold, and reports as lost the Machines LoseAtReconcile named that are Running
// or Paused. Its step is "reconcile".
func (n *Network) Reconcile(_ context.Context, machines []machine.Machine) ([]machine.MachineID, error) {
	if err := n.Enter("reconcile"); err != nil {
		return nil, err
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.reconciled = n.reconciled[:0]
	var lost []machine.MachineID
	for _, m := range machines {
		n.reconciled = append(n.reconciled, m.ID)
		if m.Network.MAC != "" {
			if _, ok := n.allocated[m.ID]; !ok {
				n.allocated[m.ID] = m.Network
			}
		}
		if (m.State == machine.StateRunning || m.State == machine.StatePaused) && n.lose[m.ID] {
			lost = append(lost, m.ID)
		}
	}
	return lost, nil
}

// LoseAtReconcile makes Reconcile report the Machine as lost if it is Running
// or Paused, as the real adapter does when its tap is gone.
func (n *Network) LoseAtReconcile(id machine.MachineID) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.lose[id] = true
}

// Reconciled lists the Machines the last Reconcile was given.
func (n *Network) Reconciled() []machine.MachineID {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]machine.MachineID(nil), n.reconciled...)
}

// LimitAddresses makes Allocate refuse with machine.ErrNoFreeAddress once
// limit Machines hold addresses; zero removes the limit.
func (n *Network) LimitAddresses(limit int) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.limit = limit
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
