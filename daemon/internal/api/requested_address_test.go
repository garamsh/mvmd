package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func requestedRequest(id, ipv4, ipv6 string) *v1alpha2.CreateMachineRequest {
	req := createRequest(id, 1, 0)
	req.RequestedIpv4, req.RequestedIpv6 = ipv4, ipv6
	return req
}

// Create Machine gives the Machine the address the request asks for, Inspect
// reports it, and a request the node cannot honour is REJECTED when the address
// is not in the node's prefix or is of the wrong form, and CONFLICT when another
// Machine or the gateway has it, each naming the address and creating nothing
// (ADR 0069).
func TestCreateMachine_GivesTheRequestedAddressAndRefusesWhatItCannot(t *testing.T) {
	ctx := context.Background()
	srv, svc := limitedServer(memory.NewSnapshotStore(), 4)

	resp, err := srv.CreateMachine(ctx, requestedRequest("first", "10.0.1.50", ""))
	require.NoError(t, err)
	assert.Contains(t, resp.GetMachine().GetAddresses(), "10.0.1.50", "the Machine has the address it asked for")
	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "first"})
	require.NoError(t, err)
	assert.Contains(t, got.GetMachine().GetAddresses(), "10.0.1.50", "Inspect reports it")

	for _, tc := range []struct {
		name       string
		ipv4, ipv6 string
		kind       v1alpha2.OutcomeKind
		names      string
	}{
		{"not an address", "10.0.1", "", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "10.0.1"},
		{"an IPv6 address in the IPv4 field", "fd00::5", "", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "fd00::5"},
		{"an IPv4 address in the IPv6 field", "", "10.0.1.7", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "10.0.1.7"},
		{"outside the node's prefix", "192.0.2.9", "", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "192.0.2.9"},
		{"a family the node does not serve", "", "fd00::5", v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "fd00::5"},
		{"the gateway", "10.0.255.254", "", v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, "10.0.255.254"},
		{"held by another Machine", "10.0.1.50", "", v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, "10.0.1.50"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateMachine(ctx, requestedRequest("second", tc.ipv4, tc.ipv6))
			assert.Equal(t, tc.kind, v1alpha2.KindOf(err))
			assert.Contains(t, status.Convert(err).Message(), tc.names)
			all, err := svc.List(ctx)
			require.NoError(t, err)
			assert.Len(t, all, 1, "no Machine was made")
		})
	}

	_, err = srv.CreateMachine(ctx, requestedRequest("second", "10.0.1.51", ""))
	require.NoError(t, err, "control: a free address inside the prefix is given")
	_, err = srv.CreateMachine(ctx, requestedRequest("third", "", ""))
	require.NoError(t, err, "control: with none asked for the node chooses")
}
