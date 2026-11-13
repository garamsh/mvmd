package machine

import (
	"net/netip"
)

// MaxRoutedPrefixes is how many routed prefixes a Machine may own (ADR 0061).
const MaxRoutedPrefixes = 4

// The shortest routed prefix a Machine may own (ADR 0061): a routed prefix is
// a block of the Machine's own, not a whole network.
const (
	minRoutedBits4 = 16
	minRoutedBits6 = 48
)

// ValidateRoutedPrefixes checks the form of a Machine's routed prefixes, which
// needs no knowledge of the node: at most MaxRoutedPrefixes, each a prefix
// with the host bits clear, IPv4 from /16 to /32 or IPv6 from /48 to /128, not
// loopback, link-local, multicast or unspecified space, and none overlapping
// another of the list. A failure is an *Error of kind ErrInvalid naming the
// prefix.
func ValidateRoutedPrefixes(prefixes []netip.Prefix) error {
	if len(prefixes) > MaxRoutedPrefixes {
		return fail(ErrInvalid, "a Machine owns at most %d routed prefixes, the request has %d", MaxRoutedPrefixes, len(prefixes))
	}
	for i, p := range prefixes {
		if err := validRoutedPrefix(p); err != nil {
			return err
		}
		for _, q := range prefixes[:i] {
			if p.Overlaps(q) {
				return fail(ErrInvalid, "routed prefix %s overlaps routed prefix %s of the same request", p, q)
			}
		}
	}
	return nil
}

func validRoutedPrefix(p netip.Prefix) error {
	a := p.Addr()
	switch {
	case !p.IsValid():
		return fail(ErrInvalid, "routed prefix %s is not a prefix", p)
	case a.Is4In6():
		return fail(ErrInvalid, "routed prefix %s is an IPv4-mapped IPv6 prefix; give the IPv4 form", p)
	case a.Zone() != "":
		return fail(ErrInvalid, "routed prefix %s has a zone", p)
	case p.Masked() != p:
		return fail(ErrInvalid, "routed prefix %s has host bits set; it is %s", p, p.Masked())
	case a.Is4() && p.Bits() < minRoutedBits4:
		return fail(ErrInvalid, "routed prefix %s is shorter than /%d", p, minRoutedBits4)
	case a.Is6() && p.Bits() < minRoutedBits6:
		return fail(ErrInvalid, "routed prefix %s is shorter than /%d", p, minRoutedBits6)
	case a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsMulticast() || a.IsUnspecified():
		return fail(ErrInvalid, "routed prefix %s is loopback, link-local, multicast or unspecified space", p)
	}
	return nil
}
