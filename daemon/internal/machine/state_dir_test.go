package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Delete removes what the supervision keeps of the Machine on the host; the
// control is a second Machine, which keeps its own.
func TestDelete_RemovesTheMachinesStateDirectory(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateStopped)
	e.inState(t, "other", machine.StateStopped)
	require.True(t, e.sup.HasState("m"), "setup: the Machine has state")

	require.NoError(t, e.svc.Delete(ctx, "m"))
	assert.False(t, e.sup.HasState("m"))
	assert.True(t, e.sup.HasState("other"), "control: another Machine's state stays")
}

// A removal that fails fails Delete as FAILED naming the cause, keeps the
// record so the identifier is not free, and a repeat succeeds.
func TestDelete_AFailedStateRemovalKeepsTheRecordAndARepeatSucceeds(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateStopped)
	e.sup.FailAt("remove-state", errBoom)

	err := e.svc.Delete(ctx, "m")
	requireKind(t, err, machine.ErrFailed)
	assert.Contains(t, err.Error(), errBoom.Error())
	e.requireState(t, "m", machine.StateStopped)
	e.requireStored(t, "m", machine.StateStopped)
	assert.True(t, e.sup.HasState("m"))
	requireKind(t, e.svc.Create(ctx, "m", request("again"), baseSpec()), machine.ErrConflict)

	e.sup.Heal("remove-state")
	require.NoError(t, e.svc.Delete(ctx, "m"))
	e.requireNoMachine(t, "m")
	assert.False(t, e.sup.HasState("m"))
}

// The start-up sweep removes the state of a Machine the State Store does not
// hold; the control is a Machine it holds, whose state stays.
func TestRestore_RemovesTheStateOfAMachineTheStoreDoesNotHold(t *testing.T) {
	e := newEnv()
	e.inState(t, "kept", machine.StateStopped)
	e.sup.KeepState("orphan")
	require.True(t, e.sup.HasState("orphan"), "setup: the orphan has state")

	e.reopened(t)
	assert.False(t, e.sup.HasState("orphan"))
	assert.True(t, e.sup.HasState("kept"), "control: a held Machine keeps its state")
}

// A sweep that cannot remove a directory fails Restore naming the Machine.
func TestRestore_ASweepThatFailsIsReported(t *testing.T) {
	e := newEnv()
	e.sup.KeepState("orphan")
	e.sup.FailAt("remove-state", errBoom)

	err := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events).Restore(context.Background())
	require.ErrorIs(t, err, errBoom)
	assert.Contains(t, err.Error(), `"orphan"`)
}
