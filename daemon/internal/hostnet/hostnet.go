// Package hostnet is the machine.Network adapter for a Linux node: one routed,
// dual-stack tap per Machine and one nftables table mvmd owns, as ADRs 0011,
// 0012 and 0015 decide. It talks to the kernel through netlink only
// (github.com/vishvananda/netlink for links, addresses and routes,
// github.com/google/nftables for the rules) and runs no external command.
//
// Nothing in the adapter is durable. A Machine's identity and policy are the
// inside's, held in its records, and Reconcile takes them back after a restart
// before any call is served. What persists in the kernel is the tap and the
// table, so a running Machine stays filtered while mvmd is down.
package hostnet

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Config is what the node's configuration says about its network (ADR 0011).
type Config struct {
	// IPv4Prefix and IPv6Prefix are the node's prefixes. An invalid one means
	// the node does not serve that family, and its taps drop it entirely.
	IPv4Prefix, IPv6Prefix netip.Prefix
	// IPv4Gateway is the host side of every tap, inside IPv4Prefix. It is
	// required when the node serves IPv4. The IPv6 gateway is always fe80::1.
	IPv4Gateway netip.Addr
	// Resolvers are the resolvers the guest is told.
	Resolvers []netip.Addr
	// VMMUIDRange is the node's range of VMM users, whose own sockets may send
	// no IP traffic (ADR 0015).
	VMMUIDRange machine.UIDRange
	// NoMasquerade are destination prefixes, each in a family the node serves,
	// that a Machine's traffic reaches with its own source address (ADR 0049).
	NoMasquerade []netip.Prefix
}

// Network implements machine.Network on the host it runs on.
type Network struct {
	cfg Config

	// mu serialises every call: each is a short sequence of kernel changes
	// that must not interleave with another's.
	mu   sync.Mutex
	held map[machine.MachineID]machine.NetworkIdentity
}

var _ machine.Network = (*Network)(nil)

// New checks the configuration and returns an adapter that holds nothing. The
// kernel is not touched until the first call.
func New(cfg Config) (*Network, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &Network{cfg: cfg, held: make(map[machine.MachineID]machine.NetworkIdentity)}, nil
}

// Validate refuses a configuration no node can serve.
func (c Config) Validate() error {
	v4, v6 := c.IPv4Prefix.IsValid(), c.IPv6Prefix.IsValid()
	switch {
	case !v4 && !v6:
		return errors.New("the node serves neither IPv4 nor IPv6")
	case v4 && !c.IPv4Prefix.Addr().Is4():
		return fmt.Errorf("IPv4 prefix %s is not an IPv4 prefix", c.IPv4Prefix)
	case v6 && (!c.IPv6Prefix.Addr().Is6() || c.IPv6Prefix.Addr().Is4In6()):
		return fmt.Errorf("IPv6 prefix %s is not an IPv6 prefix", c.IPv6Prefix)
	case v4 && !c.IPv4Prefix.Contains(c.IPv4Gateway):
		return fmt.Errorf("IPv4 gateway %s is not inside %s", c.IPv4Gateway, c.IPv4Prefix)
	case v4 && c.IPv4Prefix.Bits() > 29:
		return fmt.Errorf("IPv4 prefix %s is too small to hold a gateway and a Machine", c.IPv4Prefix)
	case v6 && c.IPv6Prefix.Bits() > 120:
		return fmt.Errorf("IPv6 prefix %s is too small to hold Machines", c.IPv6Prefix)
	}
	for _, p := range []netip.Prefix{c.IPv4Prefix, c.IPv6Prefix} {
		if err := usablePrefix(p); err != nil {
			return err
		}
	}
	if v4 {
		p := c.IPv4Prefix.Masked()
		if c.IPv4Gateway == p.Addr() || !p.Contains(c.IPv4Gateway.Next()) {
			return fmt.Errorf("IPv4 gateway %s is the network or broadcast address of %s", c.IPv4Gateway, c.IPv4Prefix)
		}
	}
	for _, r := range c.Resolvers {
		if !r.IsValid() {
			return errors.New("a resolver address is invalid")
		}
	}
	for _, p := range c.NoMasquerade {
		a := p.Addr()
		switch {
		case !p.IsValid():
			return errors.New("a no-masquerade prefix is invalid")
		case a.Is4() && !v4:
			return fmt.Errorf("no-masquerade prefix %s is IPv4 and the node does not serve IPv4", p)
		case a.Is6() && (!v6 || a.Is4In6()):
			return fmt.Errorf("no-masquerade prefix %s is IPv6 and the node does not serve IPv6", p)
		}
	}
	return c.VMMUIDRange.Validate()
}

// usablePrefix refuses a prefix Machines cannot be addressed from: all
// addresses (nothing is outside it, so nothing is masqueraded), or loopback,
// link-local or multicast space. An invalid prefix is a family not served.
func usablePrefix(p netip.Prefix) error {
	if !p.IsValid() {
		return nil
	}
	a := p.Masked().Addr()
	switch {
	case p.Bits() == 0:
		return fmt.Errorf("prefix %s holds every address", p)
	case a.IsLoopback(), a.IsLinkLocalUnicast(), a.IsMulticast(), a.IsUnspecified():
		return fmt.Errorf("prefix %s is loopback, link-local, multicast or unspecified space", p)
	}
	return nil
}

// Allocate implements machine.Network. It chooses the MAC and one address per
// family the node serves, and returns what it chose before for the same
// Machine. It holds routed (ADR 0061) with them, after refusing a family the
// node does not serve (machine.ErrInvalid) and an overlap with the node's
// Machine prefix or what another Machine holds (machine.ErrConflict).
func (n *Network) Allocate(ctx context.Context, id machine.MachineID, mac string, routed ...netip.Prefix) (machine.NetworkIdentity, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if have, ok := n.held[id]; ok {
		return have, nil
	}
	if err := n.checkRouted(id, routed); err != nil {
		return machine.NetworkIdentity{}, err
	}
	identity, err := n.allocate(mac)
	if err != nil {
		return machine.NetworkIdentity{}, err
	}
	identity.RoutedPrefixes = slices.Clone(routed)
	n.held[id] = identity
	return identity, nil
}

// Apply implements machine.Network.
func (n *Network) Apply(ctx context.Context, id machine.MachineID, user machine.User, policy machine.NetworkPolicy) (string, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	identity, ok := n.held[id]
	if !ok {
		return "", fmt.Errorf("Machine %q holds no network identity on this node", id)
	}
	m, err := n.machineNet(id, identity, user, policy)
	if err != nil {
		return "", err
	}
	if err := n.apply(ctx, m); err != nil {
		return "", fmt.Errorf("apply the network of Machine %q: %w", id, err)
	}
	return m.tap, nil
}

// Withdraw implements machine.Network.
func (n *Network) Withdraw(ctx context.Context, id machine.MachineID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.withdraw(tapName(id)); err != nil {
		return fmt.Errorf("withdraw the network of Machine %q: %w", id, err)
	}
	return nil
}

// Release implements machine.Network.
func (n *Network) Release(ctx context.Context, id machine.MachineID) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.withdraw(tapName(id)); err != nil {
		return fmt.Errorf("release the network of Machine %q: %w", id, err)
	}
	delete(n.held, id)
	return nil
}
