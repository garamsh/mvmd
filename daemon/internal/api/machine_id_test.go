package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// The client's check ahead of a call is non-empty only; the domain's refusal
// of an identifier that names no file reaches it as rejected.
func TestMachineIdentifierForm_ReachesTheClientAsRejected(t *testing.T) {
	ctx := context.Background()
	srv, svc := limitedServer(memory.NewSnapshotStore(), 4)
	checked := createRequest("a/b", 1, 0)
	checked.ImageDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	require.NoError(t, checked.Validate(), "the setup: the call passes the client's check")

	_, err := srv.CreateMachine(ctx, createRequest("a/b", 1, 0))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))

	_, err = srv.CreateMachine(ctx, createRequest("..", 1, 0))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	all, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, all, "no Machine was made")

	_, err = srv.CreateMachine(ctx, createRequest("a.b_C-1", 1, 0))
	require.NoError(t, err, "control: an identifier everything accepts")

	require.NoError(t, svc.Start(ctx, "a.b_C-1"))
	require.NoError(t, svc.Snapshot(ctx, "a.b_C-1", "snap", "r-snap"))
	_, err = srv.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{MachineId: "x/y", RequestId: "r-x", SnapshotId: "snap"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	_, err = srv.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{MachineId: "x.y", RequestId: "r-x", SnapshotId: "snap"})
	require.NoError(t, err, "control: a restore under an identifier everything accepts")
}
