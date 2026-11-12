package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/nstest"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func capacityOf(t *testing.T, n *node) *v1alpha2.NodeCapacity {
	t.Helper()
	info, err := n.client.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.NotNil(t, info.GetCapacity())
	return info.GetCapacity()
}

// The wired node reports the size of its VMM user range as the most Machines
// it can hold, the Machines it holds as they come and go, and refuses the one
// past the limit as precondition failed.
func TestRuntimeInfo_ReportsTheMachineLimitAndCountOfTheWiredNode(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.VMMUIDRange = app.VMMUIDRangeConfig{First: 1000, Count: 2}
	n := start(t, cfg)
	n.app.Substitutes.Images.Provide(digest)

	c := capacityOf(t, n)
	assert.EqualValues(t, 2, c.GetMachineLimit(), "the UID range's count")
	assert.EqualValues(t, 0, c.GetMachines())

	for _, id := range []string{"a", "b"} {
		_, err := n.client.CreateMachine(ctx, create(id))
		require.NoError(t, err, id)
	}
	c = capacityOf(t, n)
	assert.EqualValues(t, 2, c.GetMachineLimit())
	assert.EqualValues(t, 2, c.GetMachines(), "a Machine holds a record from its Create")

	_, err := n.client.CreateMachine(ctx, create("c"))
	requireKind(t, err, precondition)

	// The control: a slot freed, the same call is accepted and the count follows.
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "a"})
	require.NoError(t, err)
	assert.EqualValues(t, 1, capacityOf(t, n).GetMachines())
	_, err = n.client.CreateMachine(ctx, create("c"))
	require.NoError(t, err)
	assert.EqualValues(t, 2, capacityOf(t, n).GetMachines())
}

// With the real network adapter the limit is the least of the user range and
// the addresses of each served family's prefix, and the node refuses the
// Machine past it as precondition failed.
func TestRuntimeInfo_TheMachineLimitIsBoundedByTheAddressesOfTheNetwork(t *testing.T) {
	nstest.Run(t, func(t *testing.T) {
		ctx := context.Background()
		cfg := networkConfigured(t)
		// 10.78.0.0/29 holds .1 to .6 and the gateway is .1: five addresses.
		cfg.Network.IPv4Prefix = "10.78.0.0/29"
		n := start(t, cfg)
		n.app.Substitutes.Images.Provide(digest)

		assert.EqualValues(t, 5, capacityOf(t, n).GetMachineLimit(), "the five addresses, under the user range's 65536")

		for _, id := range []string{"a", "b", "c", "d", "e"} {
			_, err := n.client.CreateMachine(ctx, create(id))
			require.NoError(t, err, id)
		}
		_, err := n.client.CreateMachine(ctx, create("f"))
		requireKind(t, err, precondition)

		_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		_, err = n.client.CreateMachine(ctx, create("f"))
		require.NoError(t, err, "the control: a freed address is accepted")
	})
}

// The wired node reports the prefixes of its network section, and a node with
// none reports none.
func TestRuntimeInfo_ReportsTheMachinePrefixesOfTheWiredNode(t *testing.T) {
	prefixesOf := func(n *node) []string {
		info, err := n.client.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
		require.NoError(t, err)
		return info.GetMachinePrefixes()
	}
	assert.Empty(t, prefixesOf(start(t, testConfig(t))), "no network section")

	nstest.Run(t, func(t *testing.T) {
		n := start(t, networkConfigured(t))
		assert.Equal(t, []string{"10.78.0.0/24", "fd78::/64"}, prefixesOf(n))
	})
}
