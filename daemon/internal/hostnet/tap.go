package hostnet

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strings"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

// addrGenModeNone is addr_gen_mode 1, with which ADR 0011 creates the tap so
// that fe80::1 is the host's only address on it.
const addrGenModeNone = 1

// linkOf returns the named link, or nil if there is none.
func linkOf(name string) (netlink.Link, error) {
	l, err := netlink.LinkByName(name)
	var missing netlink.LinkNotFoundError
	switch {
	case errors.As(err, &missing):
		return nil, nil
	case err != nil:
		return nil, fmt.Errorf("look up link %s: %w", name, err)
	}
	return l, nil
}

// createTap makes the Machine's tap persistent, owned by its VMM user, and
// leaves the link down with no link-local address of its own.
func createTap(m machineNet) (netlink.Link, error) {
	tap := &netlink.Tuntap{
		LinkAttrs: netlink.LinkAttrs{Name: m.tap},
		Mode:      netlink.TUNTAP_MODE_TAP,
		Flags:     netlink.TUNTAP_NO_PI | netlink.TUNTAP_VNET_HDR,
		Owner:     m.user.UID,
		Group:     m.user.GID,
	}
	if err := netlink.LinkAdd(tap); err != nil {
		return nil, fmt.Errorf("create tap %s: %w", m.tap, err)
	}
	link, err := netlink.LinkByName(m.tap)
	if err != nil {
		return nil, fmt.Errorf("look up tap %s: %w", m.tap, err)
	}
	if err := netlink.LinkSetIP6AddrGenMode(link, addrGenModeNone); err != nil {
		return link, fmt.Errorf("set addr_gen_mode on tap %s: %w", m.tap, err)
	}
	return link, nil
}

// route gives the tap the host's gateway address of each family the Machine
// holds, and routes the Machine's address and its routed prefixes to it (ADR
// 0011, ADR 0061). The link must be
// up, or the kernel refuses the route.
func route(link netlink.Link, m machineNet) error {
	index := link.Attrs().Index
	if m.ip4.IsValid() {
		if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: hostNet(m.gw4)}); err != nil {
			return fmt.Errorf("add the IPv4 gateway %s to %s: %w", m.gw4, m.tap, err)
		}
		err := netlink.RouteAdd(&netlink.Route{LinkIndex: index, Scope: netlink.SCOPE_LINK, Dst: hostNet(m.ip4)})
		if err != nil {
			return fmt.Errorf("route %s to %s: %w", m.ip4, m.tap, err)
		}
	}
	if m.ip6.IsValid() {
		gateway := &netlink.Addr{IPNet: &net.IPNet{IP: linkLocalGateway.AsSlice(), Mask: net.CIDRMask(64, 128)}, Flags: unix.IFA_F_NODAD}
		if err := netlink.AddrAdd(link, gateway); err != nil {
			return fmt.Errorf("add %s to %s: %w", linkLocalGateway, m.tap, err)
		}
		if err := netlink.RouteAdd(&netlink.Route{LinkIndex: index, Dst: hostNet(m.ip6)}); err != nil {
			return fmt.Errorf("route %s to %s: %w", m.ip6, m.tap, err)
		}
	}
	return nil
}

// routeRouted routes each of the Machine's routed prefixes to the tap (ADR
// 0061). It replaces a route that exists, so reconciliation can repeat it.
func routeRouted(link netlink.Link, m machineNet) error {
	for _, p := range m.routed {
		r := &netlink.Route{LinkIndex: link.Attrs().Index, Dst: &net.IPNet{IP: p.Addr().AsSlice(), Mask: net.CIDRMask(p.Bits(), p.Addr().BitLen())}}
		if p.Addr().Is4() {
			r.Scope = netlink.SCOPE_LINK
		}
		if err := netlink.RouteReplace(r); err != nil {
			return fmt.Errorf("route %s to %s: %w", p, m.tap, err)
		}
	}
	return nil
}

// hostNet is the single-address network of a.
func hostNet(a netip.Addr) *net.IPNet {
	return &net.IPNet{IP: a.AsSlice(), Mask: net.CIDRMask(a.BitLen(), a.BitLen())}
}

// ours lists the taps of the adapter in the namespace, by name.
func ours() (map[string]netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("list links: %w", err)
	}
	taps := make(map[string]netlink.Link)
	for _, l := range links {
		if l.Type() == "tuntap" && isTapName(l.Attrs().Name) {
			taps[l.Attrs().Name] = l
		}
	}
	return taps, nil
}

// isTapName reports whether the name has the exact form tapName makes: the
// prefix and 11 lowercase hexadecimal characters. A device of any other name,
// "mvmd0" among them, is the operator's.
func isTapName(name string) bool {
	rest, ok := strings.CutPrefix(name, tapPrefix)
	if !ok || len(rest) != tapHexLen {
		return false
	}
	return strings.Trim(rest, "0123456789abcdef") == ""
}

// deleteTap takes the link down and deletes it.
func deleteTap(link netlink.Link) error {
	name := link.Attrs().Name
	if err := netlink.LinkSetDown(link); err != nil {
		return fmt.Errorf("set %s down: %w", name, err)
	}
	if err := netlink.LinkDel(link); err != nil {
		return fmt.Errorf("delete %s: %w", name, err)
	}
	return nil
}
