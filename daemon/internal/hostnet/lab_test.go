package hostnet_test

import (
	"context"
	"net"
	"net/netip"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// port is the far end of a device the test drives: a tap the guest is on, or
// the peer of the uplink. It sends and receives whole Ethernet frames.
type port struct {
	t *testing.T
	f *os.File
	// packet is a packet socket, which also receives what it sends.
	packet bool
}

// tapPort attaches to an existing tap as its VMM does.
func tapPort(t *testing.T, name string) *port {
	t.Helper()
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	ifr, err := unix.NewIfreq(name)
	require.NoError(t, err)
	ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI)
	require.NoError(t, unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr), "attach to the tap %s", name)
	require.NoError(t, unix.SetNonblock(fd, true))
	p := &port{t: t, f: os.NewFile(uintptr(fd), name)}
	t.Cleanup(func() { _ = p.f.Close() })
	return p
}

// packetPort sees and sends every frame of the device.
func packetPort(t *testing.T, name string) *port {
	t.Helper()
	l, err := netlink.LinkByName(name)
	require.NoError(t, err)
	fd, err := unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, int(htons(unix.ETH_P_ALL)))
	require.NoError(t, err)
	require.NoError(t, unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: htons(unix.ETH_P_ALL), Ifindex: l.Attrs().Index}))
	p := &port{t: t, f: os.NewFile(uintptr(fd), name), packet: true}
	t.Cleanup(func() { _ = p.f.Close() })
	return p
}

func htons(v uint16) uint16 { return v<<8 | v>>8 }

func (p *port) send(frame []byte) {
	p.t.Helper()
	_, err := p.f.Write(frame)
	require.NoError(p.t, err)
}

// next returns the next frame received, or false if none comes within d. A
// packet socket also receives what it sends, which is skipped.
func (p *port) next(d time.Duration) ([]byte, bool) {
	buf := make([]byte, 2048)
	rc, err := p.f.SyscallConn()
	require.NoError(p.t, err)
	deadline := time.Now().Add(d)
	for {
		require.NoError(p.t, p.f.SetReadDeadline(deadline))
		var n int
		var from unix.Sockaddr
		var rerr error
		err := rc.Read(func(fd uintptr) bool {
			if p.packet {
				n, from, rerr = unix.Recvfrom(int(fd), buf, 0)
			} else {
				n, rerr = unix.Read(int(fd), buf)
			}
			return rerr != unix.EAGAIN
		})
		if os.IsTimeout(err) {
			return nil, false
		}
		require.NoError(p.t, err)
		require.NoError(p.t, rerr)
		if ll, ok := from.(*unix.SockaddrLinklayer); ok && ll.Pkttype == unix.PACKET_OUTGOING {
			continue
		}
		return append([]byte(nil), buf[:n]...), true
	}
}

// drain discards what has arrived.
func (p *port) drain() {
	for {
		if _, ok := p.next(20 * time.Millisecond); !ok {
			return
		}
	}
}

// receives reports whether a frame matching want comes before the time is up,
// and ignores the others.
func (p *port) receives(want func(seen) bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		f, ok := p.next(time.Until(deadline))
		if !ok {
			return false
		}
		if s, ok := parse(f); ok && want(s) {
			return true
		}
	}
	return false
}

// The lab's topology. The node serves 10.77.0.0/24 and fd77::/64, and is
// attached to the outside by a veth pair: the host holds up0 at 192.0.2.1 and
// 2001:db8:1::1, and the test drives the peer, where 192.0.2.99 and
// 2001:db8:1::99 are.
var (
	outside4 = netip.MustParseAddr("192.0.2.99")
	outside6 = netip.MustParseAddr("2001:db8:1::99")
	other4   = netip.MustParseAddr("198.51.100.9")
	other6   = netip.MustParseAddr("2001:db8:2::9")
	hostUp4  = netip.MustParseAddr("192.0.2.1")
	hostUp6  = netip.MustParseAddr("2001:db8:1::1")

	vmmUser = machine.User{UID: 4242, GID: 4242}
)

type lab struct {
	t   *testing.T
	net *hostnet.Network
	cfg hostnet.Config
	// outside is the peer of the uplink; upMAC is the host side's address and
	// outsideMAC the peer's.
	outside           *port
	upMAC, outsideMAC net.HardwareAddr
}

func dualStack() hostnet.Config {
	return hostnet.Config{
		IPv4Prefix:  netip.MustParsePrefix("10.77.0.0/24"),
		IPv4Gateway: netip.MustParseAddr("10.77.0.1"),
		IPv6Prefix:  netip.MustParsePrefix("fd77::/64"),
		Resolvers:   []netip.Addr{netip.MustParseAddr("10.77.0.2")},
		VMMUIDRange: machine.UIDRange{First: 4000, Count: 1000},
	}
}

func ipv4Only() hostnet.Config {
	c := dualStack()
	c.IPv6Prefix = netip.Prefix{}
	return c
}

func ipv6Only() hostnet.Config {
	c := dualStack()
	c.IPv4Prefix, c.IPv4Gateway, c.Resolvers = netip.Prefix{}, netip.Addr{}, nil
	return c
}

// newLab builds the topology in the namespace the test runs in and an adapter
// for a node of the configuration.
func newLab(t *testing.T, cfg hostnet.Config) *lab {
	t.Helper()
	for path, v := range map[string]string{
		"/proc/sys/net/ipv4/ip_forward":             "1",
		"/proc/sys/net/ipv6/conf/all/forwarding":    "1",
		"/proc/sys/net/ipv4/conf/all/rp_filter":     "0",
		"/proc/sys/net/ipv4/conf/default/rp_filter": "0",
	} {
		require.NoError(t, os.WriteFile(path, []byte(v), 0o644), path)
	}
	require.NoError(t, netlink.LinkAdd(&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "up0"}, PeerName: "up1"}))
	t.Cleanup(func() {
		if l, err := netlink.LinkByName("up0"); err == nil {
			_ = netlink.LinkDel(l)
		}
	})
	up0, err := netlink.LinkByName("up0")
	require.NoError(t, err)
	up1, err := netlink.LinkByName("up1")
	require.NoError(t, err)
	for _, a := range []struct {
		l netlink.Link
		p string
	}{{up0, "192.0.2.1/24"}, {up0, "2001:db8:1::1/64"}} {
		addr, err := netlink.ParseAddr(a.p)
		require.NoError(t, err)
		addr.Flags = unix.IFA_F_NODAD
		require.NoError(t, netlink.AddrAdd(a.l, addr))
	}
	require.NoError(t, netlink.LinkSetUp(up0))
	require.NoError(t, netlink.LinkSetUp(up1))
	for _, peer := range []netip.Addr{outside4, outside6, other4, other6} {
		require.NoError(t, netlink.NeighSet(&netlink.Neigh{
			LinkIndex: up0.Attrs().Index, State: netlink.NUD_PERMANENT, IP: peer.AsSlice(), HardwareAddr: up1.Attrs().HardwareAddr,
		}))
	}
	// A second outside, which the host can route to, so that a policy that
	// does not allow it is what stops a packet and not a missing route.
	for _, dst := range []string{"198.51.100.0/24", "2001:db8:2::/64"} {
		_, prefix, err := net.ParseCIDR(dst)
		require.NoError(t, err)
		require.NoError(t, netlink.RouteAdd(&netlink.Route{LinkIndex: up0.Attrs().Index, Dst: prefix, Scope: netlink.SCOPE_LINK}))
	}
	n, err := hostnet.New(cfg)
	require.NoError(t, err)
	l := &lab{t: t, net: n, cfg: cfg, outside: packetPort(t, "up1"), upMAC: up0.Attrs().HardwareAddr, outsideMAC: up1.Attrs().HardwareAddr}
	t.Cleanup(func() {
		// What the test leaves goes, so the next one starts from nothing.
		_, _ = n.Reconcile(context.Background(), nil)
	})
	return l
}

// guest is a Machine as the test sees it: what mvmd assigned, and the tap's
// far end, which the test drives as the guest.
type guest struct {
	l        *lab
	id       machine.MachineID
	identity machine.NetworkIdentity
	tap      string
	port     *port
	mac      net.HardwareAddr
	// hostMAC is the host side of the tap.
	hostMAC   net.HardwareAddr
	linkIndex int
	// ctl4 and ctl6 are where the control frame of an outcome goes, which the
	// guest's policy must allow.
	ctl4, ctl6 netip.Addr
	ip4        netip.Addr
	ip6        netip.Addr
	ll6        netip.Addr
}

// start allocates, applies the policy and attaches to the tap, which is what a
// Start does.
func (l *lab) start(id machine.MachineID, policy machine.NetworkPolicy) *guest {
	l.t.Helper()
	g := &guest{l: l, id: id, ctl4: outside4, ctl6: outside6}
	g.allocateAndApply(policy)
	return g
}

func (g *guest) allocateAndApply(policy machine.NetworkPolicy) {
	t := g.l.t
	t.Helper()
	var err error
	g.identity, err = g.l.net.Allocate(context.Background(), g.id, "")
	require.NoError(t, err)
	g.tap, err = g.l.net.Apply(context.Background(), g.id, vmmUser, policy)
	require.NoError(t, err)
	g.mac, err = net.ParseMAC(g.identity.MAC)
	require.NoError(t, err)
	for _, a := range g.identity.Addresses {
		if a.Is4() {
			g.ip4 = a
		} else {
			g.ip6 = a
		}
	}
	if g.ip6.IsValid() {
		b := g.mac
		g.ll6 = netip.AddrFrom16([16]byte{0xfe, 0x80, 8: b[0] ^ 2, b[1], b[2], 0xff, 0xfe, b[3], b[4], b[5]})
	}
	link, err := netlink.LinkByName(g.tap)
	require.NoError(t, err)
	g.hostMAC = link.Attrs().HardwareAddr
	g.port = tapPort(t, g.tap)
	g.linkIndex = link.Attrs().Index
	// The host reaches the guest without asking who has its address.
	for _, a := range []netip.Addr{g.ip4, g.ip6} {
		if a.IsValid() {
			require.NoError(t, netlink.NeighSet(&netlink.Neigh{
				LinkIndex: g.linkIndex, State: netlink.NUD_PERMANENT, IP: a.AsSlice(), HardwareAddr: g.mac,
			}))
		}
	}
}

func allow(prefix string, proto machine.Protocol, ports ...uint16) machine.NetworkRule {
	r := machine.NetworkRule{Prefix: netip.MustParsePrefix(prefix), Protocol: proto}
	for _, p := range ports {
		r.Ports = append(r.Ports, machine.PortRange{First: p, Last: p})
	}
	return r
}
