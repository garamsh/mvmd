package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func discard(machine.OutputChunk) error { return nil }

// A request ID is remembered for as long as its exec record exists, a restart
// of the daemon included (runtime-api.md §Calling rules).
func TestExecRecords_ARestartKeepsARequestIDsConflict(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	first, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)

	svc := e.reopened(t)

	_, err = svc.Exec(ctx, "m", "x", "other", echo)
	requireKind(t, err, machine.ErrConflict)
	_, err = svc.Exec(ctx, "m", "x", "r", machine.ExecSpec{Command: []string{"other"}})
	requireKind(t, err, machine.ErrConflict)
	assert.Equal(t, 1, e.guest.Calls("exec"), "a refused repeat reaches no guest")

	// Controls: the original call is answered from the record and runs
	// nothing, and an identifier that is new runs.
	again, err := svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	assert.Equal(t, first, again)
	assert.Equal(t, 1, e.guest.Calls("exec"))
	_, err = svc.Exec(ctx, "m", "y", "r2", echo)
	require.NoError(t, err)
	assert.Equal(t, 2, e.guest.Calls("exec"))
}

// A call the guest never answered is kept with its identity, so after a
// restart its repeat asks the guest again, and a conflicting one is refused.
func TestExecRecords_ARestartKeepsACallTheGuestNeverAnswered(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.LoseExecAnswer()
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	requireKind(t, err, machine.ErrFailed)

	svc := e.reopened(t)
	_, err = svc.Exec(ctx, "m", "x", "other", echo)
	requireKind(t, err, machine.ErrConflict)

	record, err := svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecStarted, record.Status)
	assert.Equal(t, 1, e.guest.Started("x"), "the command ran once")
}

// A Machine that stopped keeps the records of what it was running, and a
// restart does not change them.
func TestExecRecords_ARestartKeepsACutShortRecord(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "running", "r1", echo)
	require.NoError(t, err)
	_, err = e.svc.Exec(ctx, "m", "ended", "r2", echo)
	require.NoError(t, err)
	e.guest.Finish("ended", 3)
	_, err = e.svc.ReadExecOutput(ctx, "m", "ended", 0, discard)
	require.NoError(t, err)
	e.guest.FailAt("exec", errBoom)
	_, err = e.svc.Exec(ctx, "m", "unanswered", "r3", echo)
	requireKind(t, err, machine.ErrFailed)
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	before, err := e.svc.ReadExecOutput(ctx, "m", "running", 0, discard)
	require.NoError(t, err)
	require.Equal(t, machine.ExecCutShort, before.Status, "the setup: it was cut short before the restart")

	svc := e.reopened(t)

	after, err := svc.ReadExecOutput(ctx, "m", "running", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, before, after, "the cut-short record, with its reason")
	repeat, err := svc.Exec(ctx, "m", "running", "r1", echo)
	require.NoError(t, err)
	assert.Equal(t, before, repeat)
	unanswered, err := svc.ReadExecOutput(ctx, "m", "unanswered", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecCutShort, unanswered.Status)
	assert.Contains(t, unanswered.Reason, "never answered")

	// Control: a command that had ended keeps its exit code.
	ended, err := svc.ReadExecOutput(ctx, "m", "ended", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecRecord{ID: "ended", Status: machine.ExecExited, ExitCode: 3}, ended)
}

// A command whose Machine's VMM ended while the daemon was away is cut short
// by the restart itself, and the finding is kept for the next one.
func TestExecRecords_ARestartCutsShortWhatTheVMMLost(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.inState(t, "alive", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	_, err = e.svc.Exec(ctx, "alive", "y", "r", echo)
	require.NoError(t, err)
	require.NoError(t, e.sup.Terminate(ctx, "m"))

	svc := e.reopened(t)
	record, err := svc.ReadExecOutput(ctx, "m", "x", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecCutShort, record.Status)
	assert.Contains(t, record.Reason, "Stopped")

	next, err := svc.ReadExecOutput(ctx, "alive", "y", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecStarted, next.Status, "control: a Machine whose VMM lives keeps its command")

	again := e.reopened(t)
	record, err = again.ReadExecOutput(ctx, "m", "x", 0, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecCutShort, record.Status)
}

// A Running Machine's command that still ran is read again from its guest.
func TestExecRecords_ARestartReadsARunningCommandFromTheGuest(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	e.guest.Emit("x", machine.OutputStdout, "out")

	svc := e.reopened(t)

	var got []machine.OutputChunk
	record, err := svc.ReadExecOutput(ctx, "m", "x", 0, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, machine.ExecStarted, record.Status)
	require.Len(t, got, 1)
	assert.Equal(t, "out", string(got[0].Data), "the output comes from the guest")

	e.guest.Finish("x", 5)
	record, err = svc.ReadExecOutput(ctx, "m", "x", 3, discard)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecExited, ExitCode: 5}, record)

	// The final record is kept for the restart after.
	final, err := e.reopened(t).Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	assert.Equal(t, record, final)
}

// A deleted Machine's records go from the store, so its identifiers are free
// after a restart.
func TestExecRecords_ARestartDropsADeletedMachinesRecords(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "gone", machine.StateRunning)
	e.inState(t, "kept", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "gone", "x", "r", echo)
	require.NoError(t, err)
	_, err = e.svc.Exec(ctx, "kept", "y", "r", echo)
	require.NoError(t, err)
	require.NoError(t, e.svc.Stop(ctx, "gone", time.Second))
	require.NoError(t, e.svc.Delete(ctx, "gone"))
	_, held := e.store.Record("gone")
	require.False(t, held, "the setup: the record is gone")
	// The Machine's identifier is used again, so entries left in the store
	// would be read as the new Machine's.
	e.inState(t, "gone", machine.StateRunning)

	svc := e.reopened(t)

	record, err := svc.Exec(ctx, "gone", "x", "r2", echo)
	require.NoError(t, err, "the identifier is free")
	assert.Equal(t, machine.ExecStarted, record.Status)

	// Control: the Machine that was not deleted keeps its records.
	_, err = svc.Exec(ctx, "kept", "y", "other", echo)
	requireKind(t, err, machine.ErrConflict)
}

// An exec whose record cannot be kept is not started, and one whose answer
// cannot be kept is asked again.
func TestExecRecords_ARecordThatCannotBeKeptIsAFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("before the guest is asked", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.store.FailAt("save-exec", errBoom)

		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		assert.Zero(t, e.guest.Calls("exec"), "nothing ran")

		e.store.Heal("save-exec")
		_, err = e.svc.Exec(ctx, "m", "x", "other", echo)
		require.NoError(t, err, "the refused call left no identity behind")
	})
	t.Run("after the guest answered", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.store.FailAfter("save-exec", e.store.Calls("save-exec")+1, errBoom)

		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		require.Equal(t, 1, e.guest.Started("x"), "the setup: the command did run")

		e.store.Heal("save-exec")
		record, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecStarted, record.Status)
		assert.Equal(t, 1, e.guest.Started("x"), "the command ran once")
		stored, _ := e.store.Record("m")
		require.Len(t, stored.Execs, 1)
		assert.True(t, stored.Execs[0].Answered, "the repeat's answer is kept")
	})
	t.Run("the end of a command", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		e.guest.Finish("x", 2)
		e.store.FailAt("save-exec", errBoom)

		_, err = e.svc.ReadExecOutput(ctx, "m", "x", 0, discard)
		requireKind(t, err, machine.ErrFailed)

		e.store.Heal("save-exec")
		record, err := e.svc.ReadExecOutput(ctx, "m", "x", 0, discard)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecExited, ExitCode: 2}, record)
	})
}

// A Machine's stop is recorded with the records it cuts short.
func TestExecRecords_ACutShortRecordIsKeptWithTheStateThatCutIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))

	stored, ok := e.store.Record("m")
	require.True(t, ok)
	require.Len(t, stored.Execs, 1)
	assert.Equal(t, machine.ExecCutShort, stored.Execs[0].Record.Status, "kept with the Stopped state")
	assert.Equal(t, machine.StateStopped, stored.Machine.State)
}
