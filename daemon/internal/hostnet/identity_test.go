package hostnet

import (
	"context"
	"net"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func nodeConfig() Config {
	return Config{
		IPv4Prefix:  netip.MustParsePrefix("10.77.0.0/29"),
		IPv4Gateway: netip.MustParseAddr("10.77.0.1"),
		IPv6Prefix:  netip.MustParsePrefix("fd77::/64"),
		Resolvers:   []netip.Addr{netip.MustParseAddr("10.77.0.2")},
		VMMUIDRange: machine.UIDRange{First: 4000, Count: 1000},
	}
}

func TestConfig_RefusesWhatNoNodeCanServe(t *testing.T) {
	tests := []struct {
		name   string
		change func(c *Config)
		ok     bool
	}{
		{"as given", func(c *Config) {}, true},
		{"IPv4 alone", func(c *Config) { c.IPv6Prefix = netip.Prefix{} }, true},
		{"IPv6 alone", func(c *Config) { c.IPv4Prefix, c.IPv4Gateway = netip.Prefix{}, netip.Addr{} }, true},
		{"neither family", func(c *Config) { c.IPv4Prefix, c.IPv6Prefix = netip.Prefix{}, netip.Prefix{} }, false},
		{"an IPv6 prefix as the IPv4 one", func(c *Config) { c.IPv4Prefix = netip.MustParsePrefix("fd00::/64") }, false},
		{"an IPv4 prefix as the IPv6 one", func(c *Config) { c.IPv6Prefix = netip.MustParsePrefix("10.0.0.0/24") }, false},
		{"a gateway outside the prefix", func(c *Config) { c.IPv4Gateway = netip.MustParseAddr("192.0.2.1") }, false},
		{"no gateway for IPv4", func(c *Config) { c.IPv4Gateway = netip.Addr{} }, false},
		{"an IPv4 prefix with no room for a Machine", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix("10.77.0.0/30"), netip.MustParseAddr("10.77.0.1")
		}, false},
		{"a gateway that is the network address", func(c *Config) { c.IPv4Gateway = netip.MustParseAddr("10.77.0.0") }, false},
		{"a gateway that is the broadcast address", func(c *Config) { c.IPv4Gateway = netip.MustParseAddr("10.77.0.7") }, false},
		{"an IPv4 prefix that holds every address", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix("0.0.0.0/0"), netip.MustParseAddr("10.0.0.1")
		}, false},
		{"an IPv6 prefix that holds every address", func(c *Config) { c.IPv6Prefix = netip.MustParsePrefix("::/0") }, false},
		{"an IPv4 loopback prefix", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix("127.0.0.0/24"), netip.MustParseAddr("127.0.0.1")
		}, false},
		{"an IPv4 link-local prefix", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix("169.254.0.0/24"), netip.MustParseAddr("169.254.0.1")
		}, false},
		{"an IPv4 multicast prefix", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix("224.0.0.0/24"), netip.MustParseAddr("224.0.0.1")
		}, false},
		{"an IPv6 link-local prefix", func(c *Config) { c.IPv6Prefix = netip.MustParsePrefix("fe80::/64") }, false},
		{"an IPv6 multicast prefix", func(c *Config) { c.IPv6Prefix = netip.MustParsePrefix("ff02::/64") }, false},
		{"an invalid resolver", func(c *Config) { c.Resolvers = []netip.Addr{{}} }, false},
		{"a VMM range that includes root", func(c *Config) { c.VMMUIDRange = machine.UIDRange{First: 0, Count: 10} }, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := nodeConfig()
			tc.change(&c)
			_, err := New(c)
			if tc.ok {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}
		})
	}
}

func TestAllocate_GivesEachMachineItsOwnAddressesAndMAC(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	ctx := context.Background()

	a, err := n.Allocate(ctx, "a", "")
	require.NoError(t, err)
	b, err := n.Allocate(ctx, "b", "")
	require.NoError(t, err)

	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.3"), netip.MustParseAddr("fd77::1")}, a.Addresses,
		"the lowest free address of each prefix, skipping the network address, the gateway and the resolver")
	assert.NotEqual(t, a.Addresses, b.Addresses)
	assert.NotEqual(t, a.MAC, b.MAC)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.1"), netip.MustParseAddr("fe80::1")}, a.Gateways)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.2")}, a.Resolvers)

	hw, err := net.ParseMAC(a.MAC)
	require.NoError(t, err)
	assert.NotZero(t, hw[0]&0x02, "locally administered")
	assert.Zero(t, hw[0]&0x01, "unicast")

	again, err := n.Allocate(ctx, "a", "")
	require.NoError(t, err)
	assert.Equal(t, a, again, "a Machine that holds an identity is given it again")
}

func TestAllocate_KeepsTheMACItIsGivenAndRefusesAnother(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	kept, err := n.Allocate(context.Background(), "restored", "52:54:00:12:34:56")
	require.NoError(t, err)
	assert.Equal(t, "52:54:00:12:34:56", kept.MAC)
	for _, bad := range []string{"not-a-mac", "52:54:00:12:34", "52:54:00:12:34:56:78:9a"} {
		_, err := n.Allocate(context.Background(), machine.MachineID(bad), bad)
		assert.Error(t, err, bad)
	}
}

func TestAllocate_RunsOutOfAddressesAndSaysSo(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	// 10.77.0.0/29 holds .1 to .6; .1 and .2 belong to the node.
	for _, id := range []machine.MachineID{"a", "b", "c", "d"} {
		_, err := n.Allocate(context.Background(), id, "")
		require.NoError(t, err, id)
	}
	_, err = n.Allocate(context.Background(), "e", "")
	assert.ErrorContains(t, err, "no free address")
	_, held := n.held["e"]
	assert.False(t, held, "a refused Machine holds nothing")
}

func TestApply_RefusesAMachineThatHoldsNoIdentity(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	_, err = n.Apply(context.Background(), "unknown", machine.User{UID: 4001, GID: 4001}, machine.NetworkPolicy{})
	assert.ErrorContains(t, err, "no network identity")
}

func TestTapName_IsStableDistinctAndFitsADevice(t *testing.T) {
	assert.Equal(t, tapName("a"), tapName("a"))
	assert.NotEqual(t, tapName("a"), tapName("b"))
	assert.LessOrEqual(t, len(tapName("a-very-long-machine-identifier-indeed")), 15, "a device name is at most 15 bytes")
	assert.Contains(t, tapName("a"), tapPrefix)
}

func TestLinkLocalOf_IsTheEUI64AddressOfTheMAC(t *testing.T) {
	hw, _ := net.ParseMAC("52:54:00:12:34:56")
	assert.Equal(t, netip.MustParseAddr("fe80::5054:ff:fe12:3456"), linkLocalOf(hw))
}
