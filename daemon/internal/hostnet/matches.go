package hostnet

import (
	"encoding/binary"
	"net"
	"net/netip"

	"github.com/google/nftables"
	"github.com/google/nftables/binaryutil"
	"github.com/google/nftables/expr"
	"golang.org/x/sys/unix"
)

// The pieces a rule is spelled from. Each returns the expressions of one
// match, loading into register 1, so a rule is their concatenation followed by
// a verdict.

func ifname(name string) []byte {
	b := make([]byte, unix.IFNAMSIZ)
	copy(b, name)
	return b
}

func be16(v uint16) []byte {
	b := make([]byte, 2)
	binary.BigEndian.PutUint16(b, v)
	return b
}

func cmp(op expr.CmpOp, data []byte) *expr.Cmp {
	return &expr.Cmp{Op: op, Register: 1, Data: data}
}

func payload(base expr.PayloadBase, offset, length uint32) *expr.Payload {
	return &expr.Payload{DestRegister: 1, Base: base, Offset: offset, Len: length}
}

func iifnameIs(name string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyIIFNAME, Register: 1}, cmp(expr.CmpOpEq, ifname(name))}
}

func oifnameIs(name string) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyOIFNAME, Register: 1}, cmp(expr.CmpOpEq, ifname(name))}
}

// nfprotoIs matches the family of the packet, which an inet chain must do
// before it reads an address.
func nfprotoIs(family byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyNFPROTO, Register: 1}, cmp(expr.CmpOpEq, []byte{family})}
}

func familyOf(a netip.Addr) byte {
	if a.Is4() {
		return unix.NFPROTO_IPV4
	}
	return unix.NFPROTO_IPV6
}

// l4protoIs matches the transport protocol, found past any IPv6 extension
// headers.
func l4protoIs(proto byte) []expr.Any {
	return []expr.Any{&expr.Meta{Key: expr.MetaKeyL4PROTO, Register: 1}, cmp(expr.CmpOpEq, []byte{proto})}
}

func etherSaddrIs(mac net.HardwareAddr) []expr.Any {
	return []expr.Any{payload(expr.PayloadBaseLLHeader, 6, 6), cmp(expr.CmpOpEq, mac)}
}

func etherTypeIs(t uint16) []expr.Any {
	return []expr.Any{payload(expr.PayloadBaseLLHeader, 12, 2), cmp(expr.CmpOpEq, be16(t))}
}

// addrIn matches the destination or source address of a packet against a
// prefix, of the family the prefix is in.
func addrIn(p netip.Prefix, daddr bool) []expr.Any {
	p = p.Masked()
	if p.Bits() == 0 {
		return nil
	}
	if p.IsSingleIP() {
		return addrIs(p.Addr(), daddr)
	}
	offset, length := headerAddress(p.Addr(), daddr)
	mask := net.CIDRMask(p.Bits(), 8*int(length))
	return []expr.Any{
		payload(expr.PayloadBaseNetworkHeader, offset, length),
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: length, Mask: mask, Xor: make([]byte, length)},
		cmp(expr.CmpOpEq, p.Addr().AsSlice()),
	}
}

// addrIs matches one address in the destination or source field.
func addrIs(a netip.Addr, daddr bool) []expr.Any {
	offset, length := headerAddress(a, daddr)
	return []expr.Any{payload(expr.PayloadBaseNetworkHeader, offset, length), cmp(expr.CmpOpEq, a.AsSlice())}
}

// addrOutside matches a header address that is not in the prefix.
func addrOutside(p netip.Prefix, daddr bool) []expr.Any {
	p = p.Masked()
	offset, length := headerAddress(p.Addr(), daddr)
	return []expr.Any{
		payload(expr.PayloadBaseNetworkHeader, offset, length),
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: length, Mask: net.CIDRMask(p.Bits(), 8*int(length)), Xor: make([]byte, length)},
		cmp(expr.CmpOpNeq, p.Addr().AsSlice()),
	}
}

// headerAddress is where the source or destination address is in the IPv4 or
// IPv6 header, and its length.
func headerAddress(a netip.Addr, daddr bool) (offset, length uint32) {
	switch {
	case a.Is4() && !daddr:
		return 12, 4
	case a.Is4():
		return 16, 4
	case !daddr:
		return 8, 16
	}
	return 24, 16
}

// ip6Hoplimit255 matches the hop limit neighbour discovery needs.
func ip6Hoplimit255() []expr.Any {
	return []expr.Any{payload(expr.PayloadBaseNetworkHeader, 7, 1), cmp(expr.CmpOpEq, []byte{255})}
}

// icmpv6TypeIn matches an ICMPv6 type in the inclusive range.
func icmpv6TypeIn(first, last byte) []expr.Any {
	return []expr.Any{
		payload(expr.PayloadBaseTransportHeader, 0, 1),
		cmp(expr.CmpOpGte, []byte{first}),
		cmp(expr.CmpOpLte, []byte{last}),
	}
}

// portIn matches the destination port in the inclusive range of a TCP or UDP
// packet.
func portIn(first, last uint16) []expr.Any {
	return []expr.Any{
		payload(expr.PayloadBaseTransportHeader, 2, 2),
		cmp(expr.CmpOpGte, be16(first)),
		cmp(expr.CmpOpLte, be16(last)),
	}
}

// ctStateIs matches a packet whose connection is in any of the states, a mask
// of expr.CtStateBit values.
func ctStateIs(states uint32) []expr.Any {
	return []expr.Any{
		&expr.Ct{Register: 1, Key: expr.CtKeySTATE},
		&expr.Bitwise{SourceRegister: 1, DestRegister: 1, Len: 4, Mask: binaryutil.NativeEndian.PutUint32(states), Xor: make([]byte, 4)},
		cmp(expr.CmpOpNeq, make([]byte, 4)),
	}
}

// skuidIn matches a packet whose socket belongs to a user in the inclusive
// range. The user is in host byte order, so it is converted before it is
// compared.
func skuidIn(first, last uint32) []expr.Any {
	be := func(v uint32) []byte { return binaryutil.BigEndian.PutUint32(v) }
	return []expr.Any{
		&expr.Meta{Key: expr.MetaKeySKUID, Register: 1},
		&expr.Byteorder{SourceRegister: 1, DestRegister: 1, Op: expr.ByteorderHton, Len: 4, Size: 4},
		cmp(expr.CmpOpGte, be(first)),
		cmp(expr.CmpOpLte, be(last)),
	}
}

func verdict(kind expr.VerdictKind) []expr.Any { return []expr.Any{&expr.Verdict{Kind: kind}} }

func jump(chain string) []expr.Any {
	return []expr.Any{&expr.Verdict{Kind: expr.VerdictJump, Chain: chain}}
}

// join concatenates the expressions of several matches.
func join(parts ...[]expr.Any) []expr.Any {
	var all []expr.Any
	for _, p := range parts {
		all = append(all, p...)
	}
	return all
}

var (
	policyDrop   = nftables.ChainPolicyDrop
	policyAccept = nftables.ChainPolicyAccept
)
