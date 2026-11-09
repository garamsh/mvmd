package hostnet

import (
	"bytes"
	"encoding/binary"
	"net"
	"net/netip"
	"testing"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests build a transaction and read what it would send to the kernel,
// which needs no privilege. The integration tests in the package's external
// tests show what the kernel does with it.

// message is one netlink message of a transaction, as far as the tests read it.
type message struct {
	kind     string // "table", "chain", "rule"
	family   byte
	chain    string
	userdata string
	raw      []byte
}

// sent is the messages a function queues on a connection, in order.
func sent(t *testing.T, queue func(c *nftables.Conn)) []message {
	t.Helper()
	var out []message
	c, err := nftables.New(nftables.WithTestDial(func(req []netlink.Message) ([]netlink.Message, error) {
		for _, m := range req {
			if msg, ok := decode(t, m); ok {
				out = append(out, msg)
			}
		}
		return req, nil
	}))
	require.NoError(t, err)
	queue(c)
	require.NoError(t, c.Flush())
	return out
}

func decode(t *testing.T, m netlink.Message) (message, bool) {
	t.Helper()
	if m.Header.Type>>8 != unix.NFNL_SUBSYS_NFTABLES || len(m.Data) < 4 {
		return message{}, false
	}
	msg := message{family: m.Data[0], raw: m.Data[4:]}
	var chainAttr, userdataAttr uint16
	switch m.Header.Type & 0xff {
	case unix.NFT_MSG_NEWTABLE:
		msg.kind = "table"
		return msg, true
	case unix.NFT_MSG_NEWCHAIN:
		msg.kind, chainAttr = "chain", unix.NFTA_CHAIN_NAME
	case unix.NFT_MSG_NEWRULE:
		msg.kind, chainAttr, userdataAttr = "rule", unix.NFTA_RULE_CHAIN, unix.NFTA_RULE_USERDATA
	default:
		return message{}, false
	}
	ad, err := netlink.NewAttributeDecoder(m.Data[4:])
	require.NoError(t, err)
	for ad.Next() {
		switch ad.Type() {
		case chainAttr:
			msg.chain = ad.String()
		case userdataAttr:
			msg.userdata = string(ad.Bytes())
		}
	}
	require.NoError(t, ad.Err())
	return msg, true
}

func only(msgs []message, kind string) []message {
	var out []message
	for _, m := range msgs {
		if m.kind == kind {
			out = append(out, m)
		}
	}
	return out
}

func rulesIn(msgs []message, family byte, chain string) []message {
	var out []message
	for _, m := range only(msgs, "rule") {
		if m.chain == chain && m.family == family {
			out = append(out, m)
		}
	}
	return out
}

func testMachine(v4, v6 bool) machineNet {
	m := machineNet{
		tap:  "mvmdtest",
		mac:  net.HardwareAddr{0x02, 0x11, 0x22, 0x33, 0x44, 0x55},
		user: machine.User{UID: 4242, GID: 4242},
	}
	if v4 {
		m.ip4, m.gw4 = netip.MustParseAddr("10.77.0.3"), netip.MustParseAddr("10.77.0.1")
	}
	if v6 {
		m.ip6, m.ll6 = netip.MustParseAddr("fd77::3"), linkLocalOf(m.mac)
	}
	return m
}

func planOf(t *testing.T, m machineNet) []message {
	t.Helper()
	plan, err := newTables().plan(m)
	require.NoError(t, err)
	return sent(t, plan.add)
}

const (
	netdev = unix.NFPROTO_NETDEV
	inet   = unix.NFPROTO_INET
)

func TestPlan_AddsTheMachinesChainsBeforeTheRulesInThem(t *testing.T) {
	msgs := planOf(t, testMachine(true, true))

	var chains []string
	firstRule := -1
	for i, m := range msgs {
		switch m.kind {
		case "chain":
			assert.Equal(t, -1, firstRule, "no chain is added after a rule: a rule needs its chain to exist")
			chains = append(chains, m.chain)
		case "rule":
			if firstRule < 0 {
				firstRule = i
			}
		}
	}
	assert.Equal(t, []string{"mvmdtest", "eg_mvmdtest", "in_mvmdtest"}, chains)
	assert.Equal(t, byte(netdev), only(msgs, "chain")[0].family, "the tap's chain is in the netdev table")
	assert.Equal(t, byte(inet), only(msgs, "chain")[1].family, "the policy chains are in the inet table")

	// The jumps from the base chains come after the chains they jump to, and
	// every rule that is not in the Machine's own chains carries its tap.
	for _, r := range only(msgs, "rule") {
		switch r.chain {
		case "forward", "input", "output":
			assert.Equal(t, "mvmdtest", r.userdata, "a rule in %s is tagged with the tap, so removal finds it", r.chain)
		default:
			assert.Empty(t, r.userdata, "a rule in %s goes with its chain", r.chain)
		}
	}
}

func TestPlan_SpellsOneRulePerCheckForEachFamilyTheMachineHolds(t *testing.T) {
	tests := []struct {
		name                        string
		v4, v6                      bool
		tapRules, input, output, fw int
	}{
		// IPv4: an ARP request, an ARP reply, an IPv4 packet. IPv6: neighbour
		// discovery from two sources, two drops, and an IPv6 packet from two
		// sources.
		{"both", true, true, 3 + 6, 3, 3, 2},
		{"IPv4 only", true, false, 3, 2, 2, 2},
		{"IPv6 only", false, true, 6, 3, 3, 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			msgs := planOf(t, testMachine(tc.v4, tc.v6))
			assert.Len(t, rulesIn(msgs, netdev, "mvmdtest"), tc.tapRules)
			assert.Len(t, rulesIn(msgs, inet, "input"), tc.input, "neighbour discovery from the tap is admitted only where the Machine has IPv6")
			assert.Len(t, rulesIn(msgs, inet, "output"), tc.output)
			assert.Len(t, rulesIn(msgs, inet, "forward"), tc.fw)
		})
	}
}

func TestPlan_AddsOneRuleForEachPortRangeOfARule(t *testing.T) {
	m := testMachine(true, false)
	base := len(rulesIn(planOf(t, m), inet, "eg_mvmdtest"))

	rule := machine.NetworkRule{Prefix: netip.MustParsePrefix("192.0.2.0/24"), Protocol: machine.ProtocolTCP}
	m.policy.Egress = []machine.NetworkRule{rule}
	assert.Equal(t, base+1, len(rulesIn(planOf(t, m), inet, "eg_mvmdtest")), "a rule with no ports is one rule")

	rule.Ports = []machine.PortRange{{First: 80, Last: 80}, {First: 8000, Last: 8999}}
	m.policy.Egress = []machine.NetworkRule{rule}
	assert.Equal(t, base+2, len(rulesIn(planOf(t, m), inet, "eg_mvmdtest")), "and with two ranges, two")
	assert.Equal(t, base, len(rulesIn(planOf(t, m), inet, "in_mvmdtest")), "an egress rule adds nothing to the ingress chain")
}

func TestPlan_RefusesAPolicyItCannotExpress(t *testing.T) {
	v4, v6 := netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")
	tests := []struct {
		name string
		rule machine.NetworkRule
		ok   bool
	}{
		{"ICMP with an IPv4 prefix", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolICMP}, true},
		{"ICMP with an IPv6 prefix", machine.NetworkRule{Prefix: v6, Protocol: machine.ProtocolICMP}, false},
		{"ICMPv6 with an IPv6 prefix", machine.NetworkRule{Prefix: v6, Protocol: machine.ProtocolICMPv6}, true},
		{"ICMPv6 with an IPv4 prefix", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolICMPv6}, false},
		{"ports on TCP", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolTCP, Ports: []machine.PortRange{{First: 1, Last: 65535}}}, true},
		{"ports on any protocol", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolAny, Ports: []machine.PortRange{{First: 1, Last: 1}}}, false},
		{"ports on ICMP", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolICMP, Ports: []machine.PortRange{{First: 1, Last: 1}}}, false},
		{"a range that ends before it starts", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolUDP, Ports: []machine.PortRange{{First: 9, Last: 8}}}, false},
		{"port zero", machine.NetworkRule{Prefix: v4, Protocol: machine.ProtocolUDP, Ports: []machine.PortRange{{First: 0, Last: 8}}}, false},
		{"no prefix", machine.NetworkRule{Protocol: machine.ProtocolUDP}, false},
		{"a protocol that does not exist", machine.NetworkRule{Prefix: v4, Protocol: machine.Protocol(99)}, false},
	}
	for _, tc := range tests {
		for _, egress := range []bool{true, false} {
			m := testMachine(true, true)
			if egress {
				m.policy.Egress = []machine.NetworkRule{tc.rule}
			} else {
				m.policy.Ingress = []machine.NetworkRule{tc.rule}
			}
			_, err := newTables().plan(m)
			if tc.ok {
				assert.NoError(t, err, "%s, egress %v", tc.name, egress)
			} else {
				assert.Error(t, err, "%s, egress %v", tc.name, egress)
			}
		}
	}
}

func TestPlan_NamesTheMachinesOwnAddressesAtItsTap(t *testing.T) {
	m := testMachine(true, true)
	var tap []byte
	for _, r := range rulesIn(planOf(t, m), netdev, "mvmdtest") {
		tap = append(tap, r.raw...)
	}
	for name, want := range map[string][]byte{
		"MAC":               m.mac,
		"IPv4 address":      m.ip4.AsSlice(),
		"IPv4 gateway":      m.gw4.AsSlice(),
		"IPv6 address":      m.ip6.AsSlice(),
		"EUI-64 link-local": m.ll6.AsSlice(),
	} {
		assert.True(t, bytes.Contains(tap, want), "the tap's rules name the Machine's %s", name)
	}
	other := m
	other.ip4 = netip.MustParseAddr("10.77.0.9")
	var otherTap []byte
	for _, r := range rulesIn(planOf(t, other), netdev, "mvmdtest") {
		otherTap = append(otherTap, r.raw...)
	}
	assert.False(t, bytes.Contains(otherTap, m.ip4.AsSlice()), "control: another address in the Machine names it instead")
	assert.True(t, bytes.Contains(otherTap, other.ip4.AsSlice()))
}

func TestAddBase_AddsTheTablesTheBaseChainsAndTheRulesNoMachineOwns(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cfg        Config
		masquerade int
	}{
		{"both", Config{IPv4Prefix: netip.MustParsePrefix("10.77.0.0/24"), IPv4Gateway: netip.MustParseAddr("10.77.0.1"), IPv6Prefix: netip.MustParsePrefix("fd77::/64"), VMMUIDRange: machine.UIDRange{First: 4000, Count: 1000}}, 2},
		{"IPv4 only", Config{IPv4Prefix: netip.MustParsePrefix("10.77.0.0/24"), IPv4Gateway: netip.MustParseAddr("10.77.0.1"), VMMUIDRange: machine.UIDRange{First: 4000, Count: 1000}}, 1},
		{"IPv6 only", Config{IPv6Prefix: netip.MustParsePrefix("fd77::/64"), VMMUIDRange: machine.UIDRange{First: 4000, Count: 1000}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n, err := New(tc.cfg)
			require.NoError(t, err)
			msgs := sent(t, func(c *nftables.Conn) { n.addBase(c, newTables()) })

			assert.Len(t, only(msgs, "table"), 2, "a netdev table and an inet table")
			var chains []string
			for _, c := range only(msgs, "chain") {
				chains = append(chains, c.chain)
			}
			assert.Equal(t, []string{"forward", "input", "output", "postrouting"}, chains)
			assert.Len(t, rulesIn(msgs, inet, "postrouting"), tc.masquerade, "one masquerade rule for each family the node serves")

			out := rulesIn(msgs, inet, "output")
			require.Len(t, out, 1, "the VMM users' drop")
			first, last := make([]byte, 4), make([]byte, 4)
			binary.BigEndian.PutUint32(first, 4000)
			binary.BigEndian.PutUint32(last, 4999)
			assert.True(t, bytes.Contains(out[0].raw, first) && bytes.Contains(out[0].raw, last), "it names the first and the last user of the range")
		})
	}
}
