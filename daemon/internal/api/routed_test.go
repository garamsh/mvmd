package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func routedRequest(id string, prefixes ...string) *v1alpha2.CreateMachineRequest {
	req := createRequest(id, 1, 0)
	req.RoutedPrefixes = prefixes
	return req
}

// Create Machine passes the routed prefixes on, Inspect reports them, and a
// request outside the rules is REJECTED and an overlap CONFLICT, each naming
// the prefix and creating nothing (ADR 0061).
func TestCreateMachine_RoutedPrefixesAreReportedRejectedOrConflicting(t *testing.T) {
	ctx := context.Background()
	srv, svc := limitedServer(memory.NewSnapshotStore(), 4)

	resp, err := srv.CreateMachine(ctx, routedRequest("first", "198.51.100.0/24", "2001:db8:100::/48"))
	require.NoError(t, err)
	assert.Equal(t, []string{"198.51.100.0/24", "2001:db8:100::/48"}, resp.GetMachine().GetRoutedPrefixes())
	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "first"})
	require.NoError(t, err)
	assert.Equal(t, []string{"198.51.100.0/24", "2001:db8:100::/48"}, got.GetMachine().GetRoutedPrefixes(), "Inspect reports them")

	for _, tc := range []struct {
		name     string
		prefixes []string
		kind     v1alpha2.OutcomeKind
		names    string
	}{
		{"not a prefix", []string{"198.51.100.0"}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "198.51.100.0"},
		{"host bits set", []string{"198.51.100.5/24"}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "198.51.100.5/24"},
		{"too short", []string{"198.0.0.0/15"}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "198.0.0.0/15"},
		{"too many", []string{"198.51.1.0/24", "198.51.2.0/24", "198.51.3.0/24", "198.51.4.0/24", "198.51.5.0/24"}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "at most 4"},
		{"overlaps another Machine's", []string{"198.51.100.0/25"}, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, "198.51.100.0/25"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := srv.CreateMachine(ctx, routedRequest("second", tc.prefixes...))
			assert.Equal(t, tc.kind, v1alpha2.KindOf(err))
			assert.Contains(t, status.Convert(err).Message(), tc.names)
			all, err := svc.List(ctx)
			require.NoError(t, err)
			assert.Len(t, all, 1, "no Machine was made")
		})
	}

	_, err = srv.CreateMachine(ctx, routedRequest("second", "203.0.113.0/24"))
	require.NoError(t, err, "control: a prefix that breaks no rule and overlaps none is accepted")
}

func TestRestoreMachine_RoutedPrefixesAreTheRequestsAndRefusedAsCreatesAre(t *testing.T) {
	ctx := context.Background()
	snaps := memory.NewSnapshotStore()
	srv, svc := limitedServer(snaps, 4)
	_, err := srv.CreateMachine(ctx, routedRequest("src", "198.51.100.0/24"))
	require.NoError(t, err)
	require.NoError(t, svc.Start(ctx, "src"))
	require.NoError(t, svc.Snapshot(ctx, "src", "snap", "r-snap"))

	restore := func(prefixes ...string) (*v1alpha2.RestoreMachineResponse, error) {
		return srv.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{MachineId: "copy", RequestId: "r-copy", SnapshotId: "snap", RoutedPrefixes: prefixes})
	}
	_, err = restore("198.51.100.5/24")
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Contains(t, status.Convert(err).Message(), "198.51.100.5/24")
	_, err = restore("not-a-prefix")
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	_, err = restore("198.51.100.0/25")
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(err), "the source Machine holds it")
	assert.Contains(t, status.Convert(err).Message(), "198.51.100.0/25")
	all, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 1, "no Machine was restored")

	resp, err := restore("203.0.113.0/24")
	require.NoError(t, err, "control: a free prefix is accepted")
	assert.Equal(t, []string{"203.0.113.0/24"}, resp.GetMachine().GetRoutedPrefixes())
}

// A Machine with no routed prefixes reports none.
func TestMachineToWire_AMachineWithNoRoutedPrefixesReportsNone(t *testing.T) {
	server := api.New(&creator{}, nil, nil, machine.DriverInfo{}, nil, nil)
	resp, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{MachineId: "m", RequestId: "r"})
	require.NoError(t, err)
	assert.Empty(t, resp.GetMachine().GetRoutedPrefixes())
}
