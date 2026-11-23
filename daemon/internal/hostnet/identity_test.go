package hostnet

import (
	"context"
	"fmt"
	"math"
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
		{"a no-masquerade prefix in each served family", func(c *Config) {
			c.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24"), netip.MustParsePrefix("2001:db8::/32")}
		}, true},
		{"an invalid no-masquerade prefix", func(c *Config) { c.NoMasquerade = []netip.Prefix{{}} }, false},
		{"an IPv4 no-masquerade prefix on an IPv6-only node", func(c *Config) {
			c.IPv4Prefix, c.IPv4Gateway = netip.Prefix{}, netip.Addr{}
			c.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
		}, false},
		{"an IPv6 no-masquerade prefix on an IPv4-only node", func(c *Config) {
			c.IPv6Prefix = netip.Prefix{}
			c.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("2001:db8::/32")}
		}, false},
		{"an IPv4-mapped no-masquerade prefix", func(c *Config) {
			c.NoMasquerade = []netip.Prefix{netip.MustParsePrefix("::ffff:192.0.2.0/120")}
		}, false},
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

	a, err := n.Allocate(ctx, "a", machine.Want{})
	require.NoError(t, err)
	b, err := n.Allocate(ctx, "b", machine.Want{})
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

	again, err := n.Allocate(ctx, "a", machine.Want{})
	require.NoError(t, err)
	assert.Equal(t, a, again, "a Machine that holds an identity is given it again")
}

func TestAllocate_KeepsTheMACItIsGivenAndRefusesAnother(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	kept, err := n.Allocate(context.Background(), "restored", machine.Want{MAC: "52:54:00:12:34:56"})
	require.NoError(t, err)
	assert.Equal(t, "52:54:00:12:34:56", kept.MAC)
	for _, bad := range []string{"not-a-mac", "52:54:00:12:34", "52:54:00:12:34:56:78:9a"} {
		_, err := n.Allocate(context.Background(), machine.MachineID(bad), machine.Want{MAC: bad})
		assert.Error(t, err, bad)
	}
}

func TestAllocate_RunsOutOfAddressesAndSaysSo(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	// 10.77.0.0/29 holds .1 to .6; .1 and .2 belong to the node.
	for _, id := range []machine.MachineID{"a", "b", "c", "d"} {
		_, err := n.Allocate(context.Background(), id, machine.Want{})
		require.NoError(t, err, id)
	}
	_, err = n.Allocate(context.Background(), "e", machine.Want{})
	assert.ErrorIs(t, err, machine.ErrNoFreeAddress)
	assert.ErrorContains(t, err, "10.77.0.0/29")
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

// TestConfig_MachineAddressesIsHowManyMachinesAllocateServes counts what
// Allocate serves before it refuses and compares it with the figure the
// configuration reports. The figure is the least over the families served.
func TestConfig_MachineAddressesIsHowManyMachinesAllocateServes(t *testing.T) {
	v4 := func(c *Config, prefix, gateway string, resolvers ...string) {
		c.IPv4Prefix, c.IPv4Gateway = netip.MustParsePrefix(prefix), netip.MustParseAddr(gateway)
		c.Resolvers = nil
		for _, r := range resolvers {
			c.Resolvers = append(c.Resolvers, netip.MustParseAddr(r))
		}
	}
	tests := []struct {
		name   string
		change func(*Config)
		want   uint32
	}{
		{"a /29 with a gateway and a resolver inside it", func(c *Config) { v4(c, "10.77.0.0/29", "10.77.0.1", "10.77.0.2"); c.IPv6Prefix = netip.Prefix{} }, 4},
		{"a /29 whose resolver is outside it", func(c *Config) { v4(c, "10.77.0.0/29", "10.77.0.1", "9.9.9.9"); c.IPv6Prefix = netip.Prefix{} }, 5},
		{"a /29 with the same address as gateway and resolver", func(c *Config) { v4(c, "10.77.0.0/29", "10.77.0.1", "10.77.0.1"); c.IPv6Prefix = netip.Prefix{} }, 5},
		{"an IPv6 prefix alone", func(c *Config) {
			*c = Config{IPv6Prefix: netip.MustParsePrefix("fd77::/120"), VMMUIDRange: c.VMMUIDRange}
		}, 255},
		{"both families, the IPv4 one smaller", func(c *Config) { v4(c, "10.77.0.0/29", "10.77.0.1"); c.IPv6Prefix = netip.MustParsePrefix("fd77::/64") }, 5},
		{"both families, the IPv6 one smaller", func(c *Config) {
			v4(c, "10.77.0.0/16", "10.77.0.1")
			c.IPv6Prefix = netip.MustParsePrefix("fd77::/120")
		}, 255},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := nodeConfig()
			tc.change(&c)
			n, err := New(c)
			require.NoError(t, err)
			served := uint32(0)
			for {
				_, err := n.Allocate(context.Background(), machine.MachineID(fmt.Sprintf("m%d", served)), machine.Want{})
				if err != nil {
					require.ErrorIs(t, err, machine.ErrNoFreeAddress)
					break
				}
				served++
			}
			assert.Equal(t, tc.want, served, "the setup: Allocate serves this many")
			assert.Equal(t, served, c.MachineAddresses())
		})
	}

	t.Run("a prefix too large to count stops at the largest uint32", func(t *testing.T) {
		c := Config{IPv6Prefix: netip.MustParsePrefix("fd77::/64")}
		assert.Equal(t, uint32(math.MaxUint32), c.MachineAddresses())
	})

	t.Run("an IPv4 /8 is counted without allocating it", func(t *testing.T) {
		c := Config{IPv4Prefix: netip.MustParsePrefix("10.0.0.0/8"), IPv4Gateway: netip.MustParseAddr("10.0.0.1")}
		assert.Equal(t, uint32(1<<24-3), c.MachineAddresses(), "2^24 addresses less the network, the broadcast and the gateway")
	})
}

// kindOf is the kind of the *machine.Error err is, or nil for none.
func kindOf(t *testing.T, err error) error {
	t.Helper()
	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	return failure.Kind
}

func TestAllocate_GivesTheAddressesAMachineAsksForAndChoosesTheRest(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	ctx := context.Background()
	asked4, asked6 := netip.MustParseAddr("10.77.0.6"), netip.MustParseAddr("fd77::beef")

	both, err := n.Allocate(ctx, "both", machine.Want{Addresses: []netip.Addr{asked4, asked6}})
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{asked4, asked6}, both.Addresses, "each family has the address asked for")

	only4, err := n.Allocate(ctx, "only4", machine.Want{Addresses: []netip.Addr{netip.MustParseAddr("10.77.0.5")}})
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.5"), netip.MustParseAddr("fd77::1")}, only4.Addresses,
		"a family not asked for is chosen as before: the lowest free address")

	none, err := n.Allocate(ctx, "none", machine.Want{})
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.3"), netip.MustParseAddr("fd77::2")}, none.Addresses,
		"control: with nothing asked for the choice is the node's, skipping what the others hold")

	again, err := n.Allocate(ctx, "both", machine.Want{Addresses: []netip.Addr{asked4, asked6}})
	require.NoError(t, err)
	assert.Equal(t, both, again, "a Machine that holds an identity is given it again")
}

func TestAllocate_RefusesAnAddressItCannotGiveAndTakesNothing(t *testing.T) {
	n, err := New(nodeConfig())
	require.NoError(t, err)
	ctx := context.Background()
	held, err := n.Allocate(ctx, "holder", machine.Want{Addresses: []netip.Addr{netip.MustParseAddr("10.77.0.4"), netip.MustParseAddr("fd77::4")}})
	require.NoError(t, err)

	tests := []struct {
		name   string
		asked  string
		kind   error
		reason string
	}{
		{"an IPv4 address outside the node's prefix", "192.0.2.9", machine.ErrInvalid, "outside the node's Machine prefix 10.77.0.0/29"},
		{"an IPv6 address outside the node's prefix", "fd78::1", machine.ErrInvalid, "outside the node's Machine prefix fd77::/64"},
		{"the network address", "10.77.0.0", machine.ErrInvalid, "the network address of 10.77.0.0/29"},
		{"the IPv6 network address", "fd77::", machine.ErrInvalid, "the network address of fd77::/64"},
		{"the broadcast address", "10.77.0.7", machine.ErrInvalid, "the broadcast address of 10.77.0.0/29"},
		{"the gateway", "10.77.0.1", machine.ErrConflict, "the gateway"},
		{"a resolver", "10.77.0.2", machine.ErrConflict, "a resolver"},
		{"an IPv4 address another Machine holds", "10.77.0.4", machine.ErrConflict, `held by Machine "holder"`},
		{"an IPv6 address another Machine holds", "fd77::4", machine.ErrConflict, `held by Machine "holder"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := netip.MustParseAddr(tt.asked)
			_, err := n.Allocate(ctx, "asker", machine.Want{Addresses: []netip.Addr{a}})
			require.Error(t, err)
			assert.Equal(t, tt.kind, kindOf(t, err))
			assert.ErrorContains(t, err, tt.asked, "the refusal names the address")
			assert.ErrorContains(t, err, tt.reason)
			_, took := n.held["asker"]
			assert.False(t, took, "a refused Machine holds nothing")
		})
	}

	// The control: the neighbours of every refused address are given.
	free, err := n.Allocate(ctx, "free", machine.Want{Addresses: []netip.Addr{netip.MustParseAddr("10.77.0.6"), netip.MustParseAddr("fd77::6")}})
	require.NoError(t, err)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.6"), netip.MustParseAddr("fd77::6")}, free.Addresses)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.4"), netip.MustParseAddr("fd77::4")}, held.Addresses, "the holder keeps its own")
}

func TestAllocate_RefusesAnAddressOfAFamilyTheNodeDoesNotServe(t *testing.T) {
	cfg := nodeConfig()
	cfg.IPv6Prefix = netip.Prefix{}
	n, err := New(cfg)
	require.NoError(t, err)

	_, err = n.Allocate(context.Background(), "m", machine.Want{Addresses: []netip.Addr{netip.MustParseAddr("fd77::5")}})
	require.Error(t, err)
	assert.Equal(t, machine.ErrInvalid, kindOf(t, err))
	assert.ErrorContains(t, err, "fd77::5")

	ok, err := n.Allocate(context.Background(), "m", machine.Want{Addresses: []netip.Addr{netip.MustParseAddr("10.77.0.5")}})
	require.NoError(t, err, "control: the served family is given")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.77.0.5")}, ok.Addresses)
}
