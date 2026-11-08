package machine

import (
	"context"
	"net/netip"
)

// NetworkIdentity is what mvmd assigns a Machine for its life: its guest MAC
// and one address per family the node serves.
type NetworkIdentity struct {
	MAC       string
	Addresses []netip.Addr
}

// Network is the conversation with the node's network: addresses, a tap per
// Machine, and the enforcement of the Machine's policy at that tap.
type Network interface {
	// Allocate assigns the Machine's MAC and addresses.
	Allocate(ctx context.Context, id MachineID) (NetworkIdentity, error)
	// Apply creates the Machine's tap if absent and commits the policy at it,
	// fail-closed, returning the tap's name. Repeating it changes nothing.
	Apply(ctx context.Context, id MachineID, policy NetworkPolicy) (tap string, err error)
	// Withdraw removes the tap and its rules, keeping the addresses. It is
	// called once the Machine's VMM process has ended or never began.
	// Withdrawing a Machine with no tap succeeds.
	Withdraw(ctx context.Context, id MachineID) error
	// Release removes the tap if present and frees the addresses. Releasing a
	// Machine that holds nothing succeeds.
	Release(ctx context.Context, id MachineID) error
}
