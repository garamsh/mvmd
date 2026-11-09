package hostnet_test

import (
	"encoding/binary"
	"net"
	"net/netip"
)

// The frames a guest or the outside sends, built by hand so that a test sends
// exactly what it means to, headers it would otherwise be refused included.

func ethernet(dst, src net.HardwareAddr, ethertype uint16, payload []byte) []byte {
	f := append(append([]byte{}, dst...), src...)
	f = binary.BigEndian.AppendUint16(f, ethertype)
	return append(f, payload...)
}

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = sum&0xffff + sum>>16
	}
	return ^uint16(sum)
}

func ipv4Packet(src, dst netip.Addr, proto byte, payload []byte) []byte {
	h := make([]byte, 20)
	h[0] = 0x45
	binary.BigEndian.PutUint16(h[2:], uint16(20+len(payload)))
	h[8], h[9] = 64, proto
	copy(h[12:], src.AsSlice())
	copy(h[16:], dst.AsSlice())
	binary.BigEndian.PutUint16(h[10:], checksum(h))
	return append(h, payload...)
}

func ipv6Packet(src, dst netip.Addr, next, hop byte, payload []byte) []byte {
	h := make([]byte, 40)
	h[0] = 0x60
	binary.BigEndian.PutUint16(h[4:], uint16(len(payload)))
	h[6], h[7] = next, hop
	copy(h[8:], src.AsSlice())
	copy(h[24:], dst.AsSlice())
	return append(h, payload...)
}

func udpDatagram(sport, dport uint16, data []byte) []byte {
	d := make([]byte, 8)
	binary.BigEndian.PutUint16(d, sport)
	binary.BigEndian.PutUint16(d[2:], dport)
	binary.BigEndian.PutUint16(d[4:], uint16(8+len(data)))
	return append(d, data...)
}

// pseudoChecksum is the checksum of an upper-layer message over the IPv6
// pseudo-header.
func pseudoChecksum(src, dst netip.Addr, next byte, msg []byte) uint16 {
	p := append(src.AsSlice(), dst.AsSlice()...)
	p = binary.BigEndian.AppendUint32(p, uint32(len(msg)))
	p = append(p, 0, 0, 0, next)
	return checksum(append(p, msg...))
}

// udp4 is an Ethernet frame holding an IPv4 UDP datagram.
func udp4(dst, src net.HardwareAddr, from, to netip.Addr, sport, dport uint16) []byte {
	return ethernet(dst, src, 0x0800, ipv4Packet(from, to, 17, udpDatagram(sport, dport, []byte("mvmd"))))
}

// udp6 is an Ethernet frame holding an IPv6 UDP datagram.
func udp6(dst, src net.HardwareAddr, from, to netip.Addr, sport, dport uint16) []byte {
	d := udpDatagram(sport, dport, []byte("mvmd"))
	binary.BigEndian.PutUint16(d[6:], pseudoChecksum(from, to, 17, d))
	return ethernet(dst, src, 0x86dd, ipv6Packet(from, to, 17, 64, d))
}

// icmp6 is an Ethernet frame holding an ICMPv6 message with the hop limit.
func icmp6(dst, src net.HardwareAddr, from, to netip.Addr, hop, typ byte, body []byte) []byte {
	m := append([]byte{typ, 0, 0, 0}, body...)
	binary.BigEndian.PutUint16(m[2:], pseudoChecksum(from, to, 58, m))
	return ethernet(dst, src, 0x86dd, ipv6Packet(from, to, 58, hop, m))
}

const (
	typeNeighbourSolicit = 135
	typeNeighbourAdvert  = 136
	typeRouterAdvert     = 134
)

func lladdrOption(kind byte, mac net.HardwareAddr) []byte {
	return append([]byte{kind, 1}, mac...)
}

func solicitedNodeMulticast(a netip.Addr) netip.Addr {
	b := netip.MustParseAddr("ff02::1:ff00:0").As16()
	t := a.As16()
	copy(b[13:], t[13:])
	return netip.AddrFrom16(b)
}

func multicastMAC(a netip.Addr) net.HardwareAddr {
	b := a.As16()
	return net.HardwareAddr{0x33, 0x33, b[12], b[13], b[14], b[15]}
}

// neighbourSolicit asks for target's link-layer address, from the source address.
func neighbourSolicit(src net.HardwareAddr, from, target netip.Addr, hop byte) []byte {
	to := solicitedNodeMulticast(target)
	body := append(make([]byte, 4), target.AsSlice()...)
	body = append(body, lladdrOption(1, src)...)
	return icmp6(multicastMAC(to), src, from, to, hop, typeNeighbourSolicit, body)
}

// neighbourAdvert answers a solicitation, as target at mac, to the host.
func neighbourAdvert(dst, mac net.HardwareAddr, from, to, target netip.Addr, hop byte) []byte {
	body := append([]byte{0x60, 0, 0, 0}, target.AsSlice()...) // solicited, override
	body = append(body, lladdrOption(2, mac)...)
	return icmp6(dst, mac, from, to, hop, typeNeighbourAdvert, body)
}

// routerAdvert announces the sender as a default router of lifetime 1800
// seconds, to all nodes.
func routerAdvert(src net.HardwareAddr, from netip.Addr) []byte {
	body := []byte{64, 0, 0x07, 0x08, 0, 0, 0, 0, 0, 0, 0, 0}
	body = append(body, lladdrOption(1, src)...)
	to := netip.MustParseAddr("ff02::1")
	return icmp6(multicastMAC(to), src, from, to, 255, typeRouterAdvert, body)
}

// arpFrame is an ARP message of Ethernet and IPv4.
func arpFrame(dst, src net.HardwareAddr, op uint16, sha net.HardwareAddr, spa netip.Addr, tha net.HardwareAddr, tpa netip.Addr) []byte {
	a := binary.BigEndian.AppendUint16(nil, 1)
	a = binary.BigEndian.AppendUint16(a, 0x0800)
	a = append(a, 6, 4)
	a = binary.BigEndian.AppendUint16(a, op)
	a = append(a, sha...)
	a = append(a, spa.AsSlice()...)
	a = append(a, tha...)
	a = append(a, tpa.AsSlice()...)
	return ethernet(dst, src, 0x0806, a)
}

// What a received frame says, for matching.
type seen struct {
	ethertype uint16
	src, dst  netip.Addr
	proto     byte
	sport     uint16
	dport     uint16
	icmpType  byte
	hop       byte
	// arpOp is the operation of an ARP message, whose src and dst are its
	// sender and target protocol addresses.
	arpOp uint16
}

func parse(f []byte) (seen, bool) {
	if len(f) < 14 {
		return seen{}, false
	}
	s := seen{ethertype: binary.BigEndian.Uint16(f[12:])}
	p := f[14:]
	switch s.ethertype {
	case 0x0806:
		if len(p) < 28 {
			return s, false
		}
		s.arpOp = binary.BigEndian.Uint16(p[6:])
		s.src, _ = netip.AddrFromSlice(p[14:18])
		s.dst, _ = netip.AddrFromSlice(p[24:28])
	case 0x0800:
		if len(p) < 20 {
			return s, false
		}
		s.src, _ = netip.AddrFromSlice(p[12:16])
		s.dst, _ = netip.AddrFromSlice(p[16:20])
		s.proto, s.hop = p[9], p[8]
		if t := p[int(p[0]&0x0f)*4:]; s.proto == 17 && len(t) >= 4 {
			s.sport, s.dport = binary.BigEndian.Uint16(t), binary.BigEndian.Uint16(t[2:])
		}
	case 0x86dd:
		if len(p) < 40 {
			return s, false
		}
		s.src, _ = netip.AddrFromSlice(p[8:24])
		s.dst, _ = netip.AddrFromSlice(p[24:40])
		s.proto, s.hop = p[6], p[7]
		if t := p[40:]; s.proto == 17 && len(t) >= 4 {
			s.sport, s.dport = binary.BigEndian.Uint16(t), binary.BigEndian.Uint16(t[2:])
		} else if s.proto == 58 && len(t) >= 1 {
			s.icmpType = t[0]
		}
	}
	return s, true
}
