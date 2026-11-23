//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A Machine created with an address it asks for has that address, which the node reports and the
// guest holds, and a Machine that asks for an address the node cannot give is refused with the
// outcome that names why: CONFLICT for the address another Machine holds and for the gateway,
// REJECTED for an address outside the node's Machine prefix (ADR 0069). A refused request makes no
// Machine, and a request that asks for nothing is given the node's choice as before.
func TestE2E_AMachineIsGivenTheAddressItAsksFor(t *testing.T) {
	const asked = "10.77.0.200"
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "requested")

	fixed := createRequest("fixed", pulled.Digest)
	fixed.RequestedIpv4 = asked
	n.runMachine(t, ctx, fixed)
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "fixed"})
	require.NoError(t, err)
	assert.Equal(t, []string{asked}, inspected.Machine.Addresses, "the node reports the address asked for")
	routes, code := n.exec(t, ctx, "fixed", "routes", "/bin/entry", "cat", "/proc/net/fib_trie")
	require.Zero(t, code)
	assert.Contains(t, routes, asked, "the guest holds the address")

	for _, tc := range []struct {
		name  string
		asked string
		kind  v1alpha2.OutcomeKind
	}{
		{"an address another Machine holds", asked, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT},
		{"the gateway", gateway, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT},
		{"an address outside the node's prefix", "192.0.2.9", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := createRequest("refused", pulled.Digest)
			req.RequestedIpv4 = tc.asked
			_, err := n.client.CreateMachine(ctx, req)
			require.Error(t, err)
			assert.Equal(t, tc.kind, v1alpha2.KindOf(err))
			assert.Contains(t, err.Error(), tc.asked, "the refusal names the address")
			assert.NotContains(t, n.listed(t, ctx), "refused", "no Machine was made")
		})
	}

	free := createRequest("chosen", pulled.Digest)
	_, err = n.client.CreateMachine(ctx, free)
	require.NoError(t, err, "control: a request that asks for nothing is given an address")
	chosen, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "chosen"})
	require.NoError(t, err)
	require.Len(t, chosen.Machine.Addresses, 1)
	assert.NotEqual(t, asked, chosen.Machine.Addresses[0], "the node's choice is not the address another Machine holds")
}
