package hostnet

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"net"
	"net/netip"
	"slices"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// tapPrefix starts the name of every tap the adapter makes, so that
// reconciliation can tell them from other devices. A name is at most 15 bytes.
const (
	tapPrefix = "mvmd"
	// tapHexLen is how many hexadecimal characters of the digest follow it.
	tapHexLen = 11
)

// tapName is the Machine's tap, derived from its identifier so that it is found
// again without any record.
func tapName(id machine.MachineID) string {
	sum := sha256.Sum256([]byte(id))
	return tapPrefix + hex.EncodeToString(sum[:])[:tapHexLen]
}

// linkLocalGateway is the host's only IPv6 address on a tap (ADR 0011).
var linkLocalGateway = netip.MustParseAddr("fe80::1")

// allocate chooses the identity of a Machine that holds none. A MAC of want is
// kept, and so is an address of it, which takes the place of the one the node
// would choose for its family. Callers hold n.mu.
func (n *Network) allocate(want machine.Want) (machine.NetworkIdentity, error) {
	asked, err := n.checkRequested(want.Addresses)
	if err != nil {
		return machine.NetworkIdentity{}, err
	}
	hw, err := n.chooseMAC(want.MAC)
	if err != nil {
		return machine.NetworkIdentity{}, err
	}
	identity := machine.NetworkIdentity{MAC: hw.String(), Resolvers: slices.Clone(n.cfg.Resolvers)}
	if p := n.cfg.IPv4Prefix; p.IsValid() {
		a, err := n.addressIn(p, asked[4])
		if err != nil {
			return machine.NetworkIdentity{}, err
		}
		identity.Addresses = append(identity.Addresses, a)
		identity.Gateways = append(identity.Gateways, n.cfg.IPv4Gateway)
	}
	if p := n.cfg.IPv6Prefix; p.IsValid() {
		a, err := n.addressIn(p, asked[6])
		if err != nil {
			return machine.NetworkIdentity{}, err
		}
		identity.Addresses = append(identity.Addresses, a)
		identity.Gateways = append(identity.Gateways, linkLocalGateway)
	}
	return identity, nil
}

// checkRequested refuses the requested addresses the node cannot give, before
// anything is chosen, and returns them by family (4 or 6). An address in a
// family the node does not serve, outside the node's Machine prefix of its
// family, or that is the prefix's network or broadcast address is
// machine.ErrInvalid, and one that is the gateway, a resolver, or an address
// another Machine holds is machine.ErrConflict. Each names the address, and the
// Machine that holds it. Callers hold n.mu.
func (n *Network) checkRequested(addrs []netip.Addr) (map[int]netip.Addr, error) {
	asked := make(map[int]netip.Addr, len(addrs))
	for _, a := range addrs {
		family, p := 4, n.cfg.IPv4Prefix
		if a.Is6() {
			family, p = 6, n.cfg.IPv6Prefix
		}
		if !p.IsValid() {
			return nil, machine.NewError(machine.ErrInvalid, "requested address %s is in a family the node does not serve", a)
		}
		p = p.Masked()
		switch {
		case !p.Contains(a):
			return nil, machine.NewError(machine.ErrInvalid, "requested address %s is outside the node's Machine prefix %s", a, p)
		case a == p.Addr():
			return nil, machine.NewError(machine.ErrInvalid, "requested address %s is the network address of %s", a, p)
		case a.Is4() && !p.Contains(a.Next()):
			return nil, machine.NewError(machine.ErrInvalid, "requested address %s is the broadcast address of %s", a, p)
		case a == n.cfg.IPv4Gateway:
			return nil, machine.NewError(machine.ErrConflict, "requested address %s is the gateway", a)
		case slices.Contains(n.cfg.Resolvers, a):
			return nil, machine.NewError(machine.ErrConflict, "requested address %s is a resolver", a)
		}
		for other, held := range n.held {
			if slices.Contains(held.Addresses, a) {
				return nil, machine.NewError(machine.ErrConflict, "requested address %s is held by Machine %q", a, other)
			}
		}
		asked[family] = a
	}
	return asked, nil
}

// addressIn is the address a Machine takes from the prefix: the one it asked for
// when it asked for one, which checkRequested has found free, and otherwise the
// lowest free one.
func (n *Network) addressIn(p netip.Prefix, asked netip.Addr) (netip.Addr, error) {
	if asked.IsValid() {
		return asked, nil
	}
	return n.freeAddress(p)
}

// checkRouted refuses the routed prefixes the node cannot give the Machine id
// (ADR 0061): a family it does not serve is machine.ErrInvalid, and a prefix
// that overlaps the node's Machine prefix, a resolver, or an address or routed
// prefix another Machine holds is machine.ErrConflict. Each names the prefix.
// Callers hold n.mu.
func (n *Network) checkRouted(id machine.MachineID, routed []netip.Prefix) error {
	for _, p := range routed {
		served := n.cfg.IPv4Prefix
		if p.Addr().Is6() {
			served = n.cfg.IPv6Prefix
		}
		if !served.IsValid() {
			return machine.NewError(machine.ErrInvalid, "routed prefix %s is in a family the node does not serve", p)
		}
		for _, node := range []netip.Prefix{n.cfg.IPv4Prefix, n.cfg.IPv6Prefix} {
			if node.IsValid() && p.Overlaps(node) {
				return machine.NewError(machine.ErrConflict, "routed prefix %s overlaps the node's Machine prefix %s", p, node)
			}
		}
		for _, r := range n.cfg.Resolvers {
			if p.Contains(r) {
				return machine.NewError(machine.ErrConflict, "routed prefix %s contains the resolver %s", p, r)
			}
		}
		for other, held := range n.held {
			if other == id {
				continue
			}
			for _, a := range held.Addresses {
				if p.Contains(a) {
					return machine.NewError(machine.ErrConflict, "routed prefix %s contains the address %s of Machine %q", p, a, other)
				}
			}
			for _, q := range held.RoutedPrefixes {
				if p.Overlaps(q) {
					return machine.NewError(machine.ErrConflict, "routed prefix %s overlaps routed prefix %s of Machine %q", p, q, other)
				}
			}
		}
	}
	return nil
}

// chooseMAC parses the MAC to keep, or makes a locally administered unicast one
// (ADR 0011) that no held identity has.
func (n *Network) chooseMAC(mac string) (net.HardwareAddr, error) {
	if mac != "" {
		hw, err := net.ParseMAC(mac)
		if err != nil || len(hw) != 6 {
			return nil, fmt.Errorf("%q is not an Ethernet MAC address", mac)
		}
		return hw, nil
	}
	taken := make(map[string]bool)
	for _, held := range n.held {
		taken[held.MAC] = true
	}
	for {
		hw := make(net.HardwareAddr, 6)
		if _, err := rand.Read(hw); err != nil {
			return nil, fmt.Errorf("choose a MAC address: %w", err)
		}
		hw[0] = hw[0]&^0x01 | 0x02
		if !taken[hw.String()] {
			return hw, nil
		}
	}
}

// freeAddress is the lowest address of the prefix that no held identity has and
// that is not the network or broadcast address, a gateway or a resolver.
func (n *Network) freeAddress(p netip.Prefix) (netip.Addr, error) {
	taken := map[netip.Addr]bool{n.cfg.IPv4Gateway: true, linkLocalGateway: true}
	for _, r := range n.cfg.Resolvers {
		taken[r] = true
	}
	for _, held := range n.held {
		for _, a := range held.Addresses {
			taken[a] = true
		}
	}
	p = p.Masked()
	for a := p.Addr().Next(); p.Contains(a); a = a.Next() {
		if taken[a] || (a.Is4() && !p.Contains(a.Next())) {
			continue
		}
		return a, nil
	}
	return netip.Addr{}, fmt.Errorf("%w: %s", machine.ErrNoFreeAddress, p)
}

// MachineAddresses is how many Machines the prefixes can address: the fewest
// assignable addresses of a family the node serves, which is every address of
// the prefix but the network and, for IPv4, the broadcast address, the gateway
// and the resolvers. It stops at the largest uint32.
func (c Config) MachineAddresses() uint32 {
	var most uint32 = math.MaxUint32
	for _, p := range []netip.Prefix{c.IPv4Prefix, c.IPv6Prefix} {
		if p.IsValid() {
			most = min(most, c.assignable(p.Masked()))
		}
	}
	return most
}

// assignable counts the addresses freeAddress can hand out from the masked
// prefix.
func (c Config) assignable(p netip.Prefix) uint32 {
	host := p.Addr().BitLen() - p.Bits()
	if host >= 32 {
		return math.MaxUint32
	}
	size := uint64(1) << host
	reserved := map[netip.Addr]bool{c.IPv4Gateway: true, linkLocalGateway: true}
	for _, r := range c.Resolvers {
		reserved[r] = true
	}
	first, last := p.Addr().Next(), p.Addr()
	if p.Addr().Is4() {
		v := p.Addr().As4()
		end := binary.BigEndian.Uint32(v[:]) + uint32(size-1)
		binary.BigEndian.PutUint32(v[:], end)
		last = netip.AddrFrom4(v)
		size -= min(size, 2)
	} else {
		size -= min(size, 1)
	}
	for a := range reserved {
		if size > 0 && p.Contains(a) && a.Compare(first) >= 0 && (!p.Addr().Is4() || a.Compare(last) < 0) {
			size--
		}
	}
	return uint32(size)
}

// machineNet is what the rules and the tap of one Machine are made from.
type machineNet struct {
	tap  string
	mac  net.HardwareAddr
	user machine.User
	// ip4 and ip6 are the Machine's addresses; an invalid one means the
	// Machine holds none of that family. ll6 is the guest's EUI-64
	// link-local address, which the tap admits beside ip6.
	ip4, ip6, ll6 netip.Addr
	gw4           netip.Addr
	// routed are the prefixes the Machine owns besides its addresses (ADR
	// 0061): the tap admits them as a source, the node routes them to the tap,
	// and the policy treats them as the Machine's own address.
	routed []netip.Prefix
	// node4 and node6 are the node's Machine prefixes, which what leaves a
	// routed prefix is masqueraded out of.
	node4, node6 netip.Prefix
	policy       machine.NetworkPolicy
}

func (n *Network) machineNet(id machine.MachineID, identity machine.NetworkIdentity, user machine.User, policy machine.NetworkPolicy) (machineNet, error) {
	hw, err := net.ParseMAC(identity.MAC)
	if err != nil || len(hw) != 6 {
		return machineNet{}, fmt.Errorf("Machine %q holds the MAC %q", id, identity.MAC)
	}
	m := machineNet{tap: tapName(id), mac: hw, user: user, policy: policy, routed: slices.Clone(identity.RoutedPrefixes), node4: n.cfg.IPv4Prefix, node6: n.cfg.IPv6Prefix}
	for _, a := range identity.Addresses {
		if a.Is4() {
			m.ip4 = a
		} else {
			m.ip6 = a
		}
	}
	for _, g := range identity.Gateways {
		if g.Is4() {
			m.gw4 = g
		}
	}
	if m.ip4.IsValid() && !m.gw4.IsValid() {
		return machineNet{}, fmt.Errorf("Machine %q holds an IPv4 address and no IPv4 gateway", id)
	}
	if m.ip6.IsValid() {
		m.ll6 = linkLocalOf(hw)
	}
	return m, nil
}

// linkLocalOf is the EUI-64 link-local address a guest kernel derives from its
// MAC by default (ADR 0011).
func linkLocalOf(hw net.HardwareAddr) netip.Addr {
	return netip.AddrFrom16([16]byte{
		0xfe, 0x80, 8: hw[0] ^ 0x02, hw[1], hw[2], 0xff, 0xfe, hw[3], hw[4], hw[5],
	})
}
