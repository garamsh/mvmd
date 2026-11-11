package machine

import (
	"context"
	"errors"
	"net/netip"
)

// ErrNoFreeAddress is what Network.Allocate returns, wrapped, when a prefix the
// node serves has no address left. It holds nothing.
var ErrNoFreeAddress = errors.New("the prefix has no free address")

// NetworkIdentity is what mvmd assigns a Machine for its life: its guest MAC
// and one address per family the node serves.
type NetworkIdentity struct {
	MAC       string
	Addresses []netip.Addr
	// Gateways and Resolvers are what the guest is told besides its addresses.
	Gateways  []netip.Addr
	Resolvers []netip.Addr
}

// guestNetwork is what a guest holding this identity is told.
func (n NetworkIdentity) guestNetwork() GuestNetwork {
	return GuestNetwork{Addresses: n.Addresses, Gateways: n.Gateways, Resolvers: n.Resolvers}
}

// Network is the conversation with the node's network: addresses, a tap per
// Machine, and the enforcement of the Machine's policy at that tap.
type Network interface {
	// Allocate assigns the Machine's addresses and its MAC. A non-empty mac is
	// the one to assign, which a Machine restored from a snapshot keeps because
	// its guest does; empty means choose one. A prefix with no free address is
	// ErrNoFreeAddress, and nothing is assigned.
	Allocate(ctx context.Context, id MachineID, mac string) (NetworkIdentity, error)
	// Apply creates the Machine's tap if absent, owned by the Machine's VMM
	// user so that the VMM attaches without privilege (ADR 0011), and commits
	// the policy at it, fail-closed, returning the tap's name. Repeating it
	// changes nothing.
	Apply(ctx context.Context, id MachineID, user User, policy NetworkPolicy) (tap string, err error)
	// Withdraw removes the tap and its rules, keeping the addresses. It is
	// called once the Machine's VMM process has ended or never began.
	// Withdrawing a Machine with no tap succeeds.
	Withdraw(ctx context.Context, id MachineID) error
	// Release removes the tap if present and frees the addresses. Releasing a
	// Machine that holds nothing succeeds.
	Release(ctx context.Context, id MachineID) error
	// Reconcile is called once, when the lifecycle has its records back after
	// a restart and has settled the Machines that were running. It is given
	// every Machine. The node's network outlives mvmd, so it brings what the
	// node holds to what the records describe: it takes the addresses the
	// Machines hold, replaces the enforcement in one step with the policies of
	// the Machines that may still have a VMM, and removes what belongs to none.
	// It returns the Running or Paused Machines whose network it could not
	// restore, which the lifecycle Faults; an error means the node's network
	// could not be brought to the records at all.
	Reconcile(ctx context.Context, machines []Machine) (lost []MachineID, err error)
}
