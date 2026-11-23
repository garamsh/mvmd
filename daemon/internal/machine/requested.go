package machine

import "net/netip"

// ValidateRequestedAddresses checks the form of a Machine's requested addresses
// (ADR 0069), which needs no knowledge of the node: each is an address, not an
// IPv4-mapped IPv6 one and with no zone, and no family is asked for twice. A
// failure is an *Error of kind ErrInvalid naming the address. Whether an address
// is inside the node's Machine prefix and free is the Network's to say.
func ValidateRequestedAddresses(addrs []netip.Addr) error {
	var v4, v6 bool
	for _, a := range addrs {
		switch {
		case !a.IsValid():
			return fail(ErrInvalid, "a requested address is not an address")
		case a.Is4In6():
			return fail(ErrInvalid, "requested address %s is an IPv4-mapped IPv6 address; give the IPv4 form", a)
		case a.Zone() != "":
			return fail(ErrInvalid, "requested address %s has a zone", a)
		case a.Is4() && v4, a.Is6() && v6:
			return fail(ErrInvalid, "requested address %s is a second address of its family; a Machine asks for one per family", a)
		}
		v4, v6 = v4 || a.Is4(), v6 || a.Is6()
	}
	return nil
}
