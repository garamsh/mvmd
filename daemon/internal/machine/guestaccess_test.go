package machine_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

var echo = machine.ExecSpec{Command: []string{"echo", "hi"}, Env: map[string]string{"A": "1"}, WorkingDir: "/tmp"}

// notRunning is every state a guest access call is refused in.
var notRunning = []machine.State{machine.StateCreated, machine.StatePaused, machine.StateStopped, machine.StateFaulted}

// refusedUnlessRunning runs the call in each state but Running, where it is
// refused as precondition failed, and in Running, where it is not.
func refusedUnlessRunning(t *testing.T, call func(e *env) error) {
	t.Helper()
	for _, from := range notRunning {
		t.Run(from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			requireKind(t, call(e), machine.ErrPreconditionFailed)
		})
	}
	t.Run("Running", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.PutFile("m", "/f", "old")
		assert.NotErrorIs(t, call(e), machine.ErrPreconditionFailed)
	})
	t.Run("an absent Machine", func(t *testing.T) {
		requireKind(t, call(newEnv()), machine.ErrNotFound)
	})
}

// Row "Exec", column Needs.
func TestExec_Needs(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		return err
	})

	t.Run("an exec identifier, request ID and command", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		for _, tt := range []struct {
			exec    machine.ExecID
			request machine.RequestID
			spec    machine.ExecSpec
		}{{"", "r", echo}, {"x", "", echo}, {"x", "r", machine.ExecSpec{}}} {
			_, err := e.svc.Exec(ctx, "m", tt.exec, tt.request, tt.spec)
			requireKind(t, err, machine.ErrInvalid)
		}
		assert.Zero(t, e.guest.Calls("exec"))
	})
	t.Run("not the identifier the agent keeps for the entrypoint", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		_, err := e.svc.Exec(ctx, "m", "entrypoint", "r", echo)
		requireKind(t, err, machine.ErrInvalid)
		assert.Zero(t, e.guest.Calls("exec"), "nothing is recorded as the client's command")

		// Control: a neighbouring identifier is accepted.
		_, err = e.svc.Exec(ctx, "m", "entrypoint2", "r", echo)
		require.NoError(t, err)
	})
}

// A Machine's exec records go with it: after Delete the identifiers are free.
func TestExec_RecordsGoWithTheirMachine(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	_, err = e.svc.Exec(ctx, "m", "x", "other", echo)
	require.Error(t, err, "the setup: while the Machine exists, the identifier is taken")

	require.NoError(t, e.svc.Delete(ctx, "m"))
	e.inState(t, "again", machine.StateRunning)
	record, err := e.svc.Exec(ctx, "again", "x", "r2", echo)
	require.NoError(t, err, "the identifier is free once its Machine is gone")
	assert.Equal(t, machine.ExecStarted, record.Status)

	// Control: another Machine's records are kept.
	_, err = e.svc.Exec(ctx, "again", "x", "r3", echo)
	requireKind(t, err, machine.ErrConflict)
}

// Row "Exec", column On success.
func TestExec_OnSuccess(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)

	record, err := e.svc.Exec(context.Background(), "m", "x", "r", echo)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecStarted}, record)
	assert.Equal(t, 1, e.guest.Started("x"))
	e.requireState(t, "m", machine.StateRunning)
}

// Row "Exec", column Repeat.
func TestExec_Repeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	first, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)

	again, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	assert.Equal(t, first, again, "the same exec record")
	assert.Equal(t, 1, e.guest.Calls("exec"), "the command is never run a second time")

	_, err = e.svc.Exec(ctx, "m", "x", "other", echo)
	requireKind(t, err, machine.ErrConflict)
	_, err = e.svc.Exec(ctx, "m", "x", "r", machine.ExecSpec{Command: []string{"other"}})
	requireKind(t, err, machine.ErrConflict)
	assert.Equal(t, 1, e.guest.Calls("exec"))

	// Control: another exec identifier runs.
	_, err = e.svc.Exec(ctx, "m", "y", "r2", echo)
	require.NoError(t, err)
	assert.Equal(t, 2, e.guest.Calls("exec"))
}

// Row "Exec", column After failure, and the record of a command whose Machine
// stops.
func TestExec_AfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a call that did not reach the guest is no outcome, and its repeat asks again", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("exec", errBoom)

		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		e.requireState(t, "m", machine.StateRunning)
		_, err = e.svc.Exec(ctx, "m", "x", "other", echo)
		requireKind(t, err, machine.ErrConflict) // the call's identity is kept

		e.guest.Heal("exec")
		record, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecStarted, record.Status, "the repeat reached the guest, whose record it is")
		assert.Equal(t, 1, e.guest.Started("x"))
	})
	t.Run("an answer that was lost leaves the command running, and the repeat returns its record", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.LoseExecAnswer()

		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		require.Equal(t, 1, e.guest.Started("x"), "the setup: the command did run")

		record, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecStarted, record.Status, "not a record that says it never started")
		assert.Equal(t, 1, e.guest.Started("x"), "the command ran once")
		assert.Equal(t, 2, e.guest.Calls("exec"))
	})
	t.Run("a guest that answers the command did not start says so, and repeats so", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.RefuseExec("x", "no such file")

		record, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecNotStarted, Reason: "no such file"}, record)
		again, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		assert.Equal(t, record, again)
		assert.Equal(t, 1, e.guest.Calls("exec"), "the answer is the outcome, and the guest is not asked again")
		assert.Zero(t, e.guest.Started("x"))
	})
	t.Run("a Machine that stops while the command runs cuts it short", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		_, err := e.svc.Exec(ctx, "m", "running", "r1", echo)
		require.NoError(t, err)
		_, err = e.svc.Exec(ctx, "m", "ended", "r2", echo)
		require.NoError(t, err)
		e.guest.Finish("ended", 0)
		_, err = e.svc.ReadExecOutput(ctx, "m", "ended", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err)

		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))

		record, err := e.svc.ReadExecOutput(ctx, "m", "running", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err)
		assert.Equal(t, machine.ExecCutShort, record.Status)
		assert.Contains(t, record.Reason, "Stopped", "and why")
		repeat, err := e.svc.Exec(ctx, "m", "running", "r1", echo)
		require.NoError(t, err)
		assert.Equal(t, record, repeat, "the repeat is the same record")

		// Control: a command that had ended keeps its exit status.
		kept, err := e.svc.ReadExecOutput(ctx, "m", "ended", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err)
		assert.Equal(t, machine.ExecExited, kept.Status)
	})
	t.Run("a Machine that stops settles a start the guest never answered", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("exec", errBoom)
		_, err := e.svc.Exec(ctx, "m", "unanswered", "r1", echo)
		requireKind(t, err, machine.ErrFailed)
		e.guest.Heal("exec")
		_, err = e.svc.Exec(ctx, "m", "answered", "r2", echo)
		require.NoError(t, err)

		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))

		unanswered, err := e.svc.ReadExecOutput(ctx, "m", "unanswered", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err, "the record can be read once the Machine has stopped")
		assert.Equal(t, machine.ExecCutShort, unanswered.Status)
		assert.Contains(t, unanswered.Reason, "never answered", "and says whether the command ran is not known")

		// Control: a running command that was answered is cut short the same way.
		answered, err := e.svc.ReadExecOutput(ctx, "m", "answered", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err)
		assert.Equal(t, machine.ExecCutShort, answered.Status)
		assert.NotContains(t, answered.Reason, "never answered")

		// A repeat of the unanswered call is the settled record, not a new start.
		repeat, err := e.svc.Exec(ctx, "m", "unanswered", "r1", echo)
		require.NoError(t, err)
		assert.Equal(t, unanswered, repeat)
	})
	t.Run("a Machine that becomes Faulted cuts it short as well", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		require.NoError(t, err)
		e.sup.FailAt("terminate", errBoom)
		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)

		record, err := e.svc.ReadExecOutput(ctx, "m", "x", 0, func(machine.OutputChunk) error { return nil })
		require.NoError(t, err)
		assert.Equal(t, machine.ExecCutShort, record.Status)
	})
}

func collect(chunks *[]machine.OutputChunk) func(machine.OutputChunk) error {
	return func(c machine.OutputChunk) error { *chunks = append(*chunks, c); return nil }
}

// Row "Read exec output", column Needs.
func TestReadExecOutput_Needs(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.created(t, "other")
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)

	_, err = e.svc.ReadExecOutput(ctx, "m", "nope", 0, collect(new([]machine.OutputChunk)))
	requireKind(t, err, machine.ErrNotFound)
	_, err = e.svc.ReadExecOutput(ctx, "other", "x", 0, collect(new([]machine.OutputChunk)))
	requireKind(t, err, machine.ErrNotFound)

	// Control: the Machine's own exec is read.
	_, err = e.svc.ReadExecOutput(ctx, "m", "x", 0, collect(new([]machine.OutputChunk)))
	require.NoError(t, err)
}

// Row "Read exec output", column Needs: a command that runs is read only while
// its Machine is Running; a Paused Machine's guest does not answer. A record
// that is final is answered from what mvmd holds.
func TestReadExecOutput_NeedsRunningWhileTheCommandRuns(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	require.NoError(t, e.svc.Pause(ctx, "m"))

	_, err = e.svc.ReadExecOutput(ctx, "m", "x", 0, collect(new([]machine.OutputChunk)))
	requireKind(t, err, machine.ErrPreconditionFailed)
	assert.Zero(t, e.guest.Calls("read-exec-output"), "the guest was not asked")

	// Control: the same read succeeds once the Machine runs again.
	require.NoError(t, e.svc.Resume(ctx, "m"))
	_, err = e.svc.ReadExecOutput(ctx, "m", "x", 0, collect(new([]machine.OutputChunk)))
	require.NoError(t, err)
}

// Row "Read exec output", columns On success and Repeat: the chunks after the
// offset, then the record once the command has ended, and a resume from any
// offset still held.
func TestReadExecOutput_OnSuccessAndRepeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	e.guest.Emit("x", machine.OutputStdout, "out1")
	e.guest.Emit("x", machine.OutputStderr, "err2")

	var got []machine.OutputChunk
	record, err := e.svc.ReadExecOutput(ctx, "m", "x", 0, collect(&got))
	require.NoError(t, err)
	assert.Equal(t, []machine.OutputChunk{
		{Stream: machine.OutputStdout, Offset: 0, Data: []byte("out1")},
		{Stream: machine.OutputStderr, Offset: 4, Data: []byte("err2")},
	}, got, "in the order received, each with its offset")
	assert.Equal(t, machine.ExecStarted, record.Status, "the command still runs")

	// A repeat from an offset resumes after it.
	got = nil
	_, err = e.svc.ReadExecOutput(ctx, "m", "x", 4, collect(&got))
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, uint64(4), got[0].Offset)

	e.guest.Finish("x", 3)
	got = nil
	record, err = e.svc.ReadExecOutput(ctx, "m", "x", 8, collect(&got))
	require.NoError(t, err)
	assert.Empty(t, got)
	assert.Equal(t, machine.ExecRecord{ID: "x", Status: machine.ExecExited, ExitCode: 3}, record, "the record carries the exit code")
}

// Row "Read exec output", column After failure: an offset no longer held is
// answered with the earliest offset held, beside a read inside the window.
func TestReadExecOutput_AnOffsetNoLongerHeld(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.guest = memory.NewGuest(8) // keeps the newest 8 bytes
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events)
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	for _, s := range []string{"aaaa", "bbbb", "cccc"} {
		e.guest.Emit("x", machine.OutputStdout, s)
	}

	_, err = e.svc.ReadExecOutput(ctx, "m", "x", 0, collect(new([]machine.OutputChunk)))
	var gone *machine.OutputGoneError
	require.ErrorAs(t, err, &gone)
	assert.Equal(t, uint64(4), gone.Earliest, "the earliest offset held")

	var got []machine.OutputChunk
	_, err = e.svc.ReadExecOutput(ctx, "m", "x", gone.Earliest, collect(&got))
	require.NoError(t, err, "a read inside the window succeeds")
	assert.Equal(t, []string{"bbbb", "cccc"}, []string{string(got[0].Data), string(got[1].Data)})
	e.requireState(t, "m", machine.StateRunning)
}

// Row "Read file".
func TestReadFile(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		return e.svc.ReadFile(ctx, "m", "/f", func([]byte) error { return nil })
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/f", "hello world")
	e.guest.PutSpecial("m", "/dev/x")

	var chunks [][]byte
	require.NoError(t, e.svc.ReadFile(ctx, "m", "/f", func(c []byte) error { chunks = append(chunks, c); return nil }))
	assert.Greater(t, len(chunks), 1, "content comes in chunks")
	assert.Equal(t, "hello world", string(bytes.Join(chunks, nil)))
	require.NoError(t, e.svc.ReadFile(ctx, "m", "/f", func([]byte) error { return nil }), "always safe to repeat")

	noop := func([]byte) error { return nil }
	requireKind(t, e.svc.ReadFile(ctx, "m", "/missing", noop), machine.ErrNotFound)
	requireKind(t, e.svc.ReadFile(ctx, "m", "/dev/x", noop), machine.ErrInvalid)
	requireKind(t, e.svc.ReadFile(ctx, "m", "", noop), machine.ErrInvalid)
	e.guest.FailAt("read-file", errBoom)
	requireKind(t, e.svc.ReadFile(ctx, "m", "/f", noop), machine.ErrFailed)
	e.requireState(t, "m", machine.StateRunning)
}

// Row "Write file".
func TestWriteFile(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		return e.svc.WriteFile(ctx, "m", "/f", bytes.NewReader([]byte("new")))
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/f", "old")

	require.NoError(t, e.svc.WriteFile(ctx, "m", "/f", bytes.NewReader([]byte("new content"))))
	got, _ := e.guest.File("m", "/f")
	assert.Equal(t, "new content", got, "the whole new content")
	require.NoError(t, e.svc.WriteFile(ctx, "m", "/f", bytes.NewReader([]byte("new content"))), "the same content: success")

	t.Run("a stream that breaks leaves the old content", func(t *testing.T) {
		broken := io.MultiReader(bytes.NewReader([]byte("part")), iotest.ErrReader(errBoom))
		require.Error(t, e.svc.WriteFile(ctx, "m", "/f", broken))
		got, _ := e.guest.File("m", "/f")
		assert.Equal(t, "new content", got, "whole old content, never part of the new")

		// Control: the same content over a stream that ends cleanly is committed.
		require.NoError(t, e.svc.WriteFile(ctx, "m", "/f", bytes.NewReader([]byte("part"))))
		got, _ = e.guest.File("m", "/f")
		assert.Equal(t, "part", got)
	})
	t.Run("a guest that cannot be reached leaves the old content", func(t *testing.T) {
		e.guest.FailAt("write-file", errBoom)
		requireKind(t, e.svc.WriteFile(ctx, "m", "/f", bytes.NewReader([]byte("lost"))), machine.ErrFailed)
		got, _ := e.guest.File("m", "/f")
		assert.Equal(t, "part", got)
	})
	t.Run("a path that is not a regular file", func(t *testing.T) {
		e.guest.Heal("write-file")
		e.guest.PutSpecial("m", "/dev/x")
		requireKind(t, e.svc.WriteFile(ctx, "m", "/dev/x", bytes.NewReader([]byte("x"))), machine.ErrInvalid)
	})
}

// Row "Patch file": applied, already applied, and neither.
func TestPatchFile(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		return e.svc.PatchFile(ctx, "m", "/f", memory.Digest([]byte("old")), memory.Digest([]byte("new")), []byte("new"))
	})

	base, expected := memory.Digest([]byte("old")), memory.Digest([]byte("new"))
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/f", "old")

	require.NoError(t, e.svc.PatchFile(ctx, "m", "/f", base, expected, []byte("new")), "at the base digest: applied")
	got, _ := e.guest.File("m", "/f")
	assert.Equal(t, "new", got)

	before := e.guest.Calls("patch-file")
	require.NoError(t, e.svc.PatchFile(ctx, "m", "/f", base, expected, []byte("something else")), "at the expected digest: success, unchanged")
	got, _ = e.guest.File("m", "/f")
	assert.Equal(t, "new", got, "an already applied patch changes nothing")
	assert.Equal(t, before+1, e.guest.Calls("patch-file"))

	e.guest.PutFile("m", "/f", "elsewhere")
	requireKind(t, e.svc.PatchFile(ctx, "m", "/f", base, expected, []byte("new")), machine.ErrPreconditionFailed)
	got, _ = e.guest.File("m", "/f")
	assert.Equal(t, "elsewhere", got, "the old content")

	requireKind(t, e.svc.PatchFile(ctx, "m", "/missing", base, expected, nil), machine.ErrNotFound)
}

// Row "Search".
func TestSearchFiles(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		_, err := searched(ctx, e.svc, "/", "**/*.log")
		return err
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/var/a.log", "")
	e.guest.PutFile("m", "/var/b.txt", "")
	e.guest.PutFile("m", "/etc/c.log", "")

	got, err := searched(ctx, e.svc, "/var/", "*.log")
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/a.log"}, got)
	all, err := searched(ctx, e.svc, "/", "**/*.log")
	require.NoError(t, err)
	assert.Equal(t, []string{"/etc/c.log", "/var/a.log"}, all, "control: a wider path finds more")

	e.guest.FailAt("search-files", errors.New("boom"))
	_, err = searched(ctx, e.svc, "/", "**/*.log")
	requireKind(t, err, machine.ErrFailed)
}

// Row "Search", a pattern that is not a valid glob: rejected and nothing
// changes, with a valid pattern of the same call as the control. A failure of
// another kind keeps its outcome.
func TestSearchFiles_RejectsAPatternThatIsNotAGlob(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/var/a.log", "")

	for _, pattern := range []string{"[a", "{a"} {
		_, err := searched(ctx, e.svc, "/var/", pattern)
		requireKind(t, err, machine.ErrInvalid)
		assert.ErrorContains(t, err, pattern, "the reason names the pattern")
	}
	got, err := searched(ctx, e.svc, "/var/", "*.log")
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/a.log"}, got, "control: a valid pattern is not refused")
	e.requireState(t, "m", machine.StateRunning)

	e.guest.FailAt("search-files", errors.New("boom"))
	_, err = searched(ctx, e.svc, "/var/", "*.log")
	requireKind(t, err, machine.ErrFailed)
}

// Row "List": the entries directly under the directory, with their kind and
// size, and the directories that hold deeper files.
func TestListFiles(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		_, err := listEntries(ctx, e.svc, "/")
		return err
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/var/a.log", "abc")
	e.guest.PutFile("m", "/var/lib/b", "")
	e.guest.PutFile("m", "/var/lib/c", "")
	e.guest.PutFile("m", "/etc/c.log", "")

	got, err := listEntries(ctx, e.svc, "/var")
	require.NoError(t, err)
	assert.Equal(t, []machine.FileEntry{
		{Path: "/var/a.log", Kind: machine.EntryFile, Size: 3},
		{Path: "/var/lib", Kind: machine.EntryDirectory},
	}, got, "a file with its size, a directory once, and nothing from below it")
	again, err := listEntries(ctx, e.svc, "/var")
	require.NoError(t, err)
	assert.Equal(t, got, again, "a repeat answers the same")
	e.requireState(t, "m", machine.StateRunning)

	_, err = listEntries(ctx, e.svc, "")
	requireKind(t, err, machine.ErrInvalid)
	_, err = listEntries(ctx, e.svc, "/missing")
	requireKind(t, err, machine.ErrNotFound)
	_, err = listEntries(ctx, e.svc, "/var/a.log")
	requireKind(t, err, machine.ErrInvalid) // a file is not a directory: the caller's error
	e.guest.FailAt("list-files", errors.New("boom"))
	_, err = listEntries(ctx, e.svc, "/var")
	requireKind(t, err, machine.ErrFailed)
	e.requireState(t, "m", machine.StateRunning)
}

// listEntries collects what ListFiles sends of Machine "m".
func listEntries(ctx context.Context, svc machine.Service, path string) ([]machine.FileEntry, error) {
	var got []machine.FileEntry
	err := svc.ListFiles(ctx, "m", path, func(run []machine.FileEntry) error {
		got = append(got, run...)
		return nil
	})
	return got, err
}

// searched collects what SearchFiles sends of Machine "m".
func searched(ctx context.Context, svc machine.Service, path, pattern string) ([]string, error) {
	var got []string
	err := svc.SearchFiles(ctx, "m", path, pattern, func(run []string) error {
		got = append(got, run...)
		return nil
	})
	return got, err
}

// Rows "Read file", "Write file", "Patch file", "List" and "Search": a path
// that is not absolute is rejected as invalid before the Guest is called, with
// the same operation on an absolute path as the control.
func TestGuestFileOperations_RejectAPathThatIsNotAbsolute(t *testing.T) {
	ctx := context.Background()
	noop := func([]byte) error { return nil }
	base, expected := memory.Digest([]byte("old")), memory.Digest([]byte("new"))
	ops := map[string]func(e *env, path string) error{
		"read file": func(e *env, path string) error { return e.svc.ReadFile(ctx, "m", path, noop) },
		"write file": func(e *env, path string) error {
			return e.svc.WriteFile(ctx, "m", path, bytes.NewReader([]byte("new")))
		},
		"patch file": func(e *env, path string) error {
			return e.svc.PatchFile(ctx, "m", path, base, expected, []byte("new"))
		},
		"list files": func(e *env, path string) error { _, err := listEntries(ctx, e.svc, path); return err },
		"search files": func(e *env, path string) error {
			_, err := searched(ctx, e.svc, path, "*")
			return err
		},
	}
	for name, op := range ops {
		t.Run(name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", machine.StateRunning)
			e.guest.PutFile("m", "/d/f", "old")

			for _, relative := range []string{"d/f", "./d/f", "d", " /d/f"} {
				err := op(e, relative)
				requireKind(t, err, machine.ErrInvalid)
				assert.ErrorContains(t, err, relative, "the reason names the path")
			}
			e.requireState(t, "m", machine.StateRunning)
			got, _ := e.guest.File("m", "/d/f")
			assert.Equal(t, "old", string(got), "nothing changed")

			absolute := "/d/f"
			if name == "list files" || name == "search files" {
				absolute = "/d"
			}
			require.NoError(t, op(e, absolute), "control: an absolute path of the same operation is not refused")
		})
	}
}

// A listing and a search are passed on in the runs the guest sends them in,
// sorted across the runs, and a failure of send ends the call.
func TestListFiles_PassesRunsOn(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	for i := range 1100 {
		e.guest.PutFile("m", fmt.Sprintf("/d/f%04d", i), "")
	}

	var sizes []int
	var last string
	require.NoError(t, e.svc.ListFiles(ctx, "m", "/d", func(run []machine.FileEntry) error {
		sizes = append(sizes, len(run))
		for _, f := range run {
			assert.Greater(t, f.Path, last, "sorted across the runs")
			last = f.Path
		}
		return nil
	}))
	assert.Equal(t, []int{512, 512, 76}, sizes)

	sizes = nil
	require.NoError(t, e.svc.SearchFiles(ctx, "m", "/d/", "f*", func(run []string) error {
		sizes = append(sizes, len(run))
		return nil
	}))
	assert.Equal(t, []int{512, 512, 76}, sizes)

	err := e.svc.ListFiles(ctx, "m", "/d", func([]machine.FileEntry) error { return errors.New("the client is gone") })
	requireKind(t, err, machine.ErrFailed)
}

// Row "Signal", column Needs.
func TestSignalExec_Needs(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		return e.svc.SignalExec(ctx, "m", "x", 15)
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)
	requireKind(t, e.svc.SignalExec(ctx, "", "x", 15), machine.ErrNotFound)
	requireKind(t, e.svc.SignalExec(ctx, "m", "", 15), machine.ErrInvalid)
	requireKind(t, e.svc.SignalExec(ctx, "m", "x", 0), machine.ErrInvalid)
	requireKind(t, e.svc.SignalExec(ctx, "m", "unknown", 15), machine.ErrNotFound)
	assert.Zero(t, e.guest.Calls("signal"), "the guest was not asked")

	// Control: a command of the Machine, and a signal number, reach the guest.
	require.NoError(t, e.svc.SignalExec(ctx, "m", "x", 15))
	assert.Equal(t, 1, e.guest.Calls("signal"))

	e.inState(t, "other", machine.StateRunning)
	requireKind(t, e.svc.SignalExec(ctx, "other", "x", 15), machine.ErrNotFound)
	assert.Equal(t, 1, e.guest.Calls("signal"), "an exec of another Machine is not signalled")
}

// Row "Signal": the entrypoint takes a signal though mvmd holds no record of it.
func TestSignalExec_TheEntrypointIsSignalledWithoutARecord(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.guest.Exec(ctx, "m", "entrypoint", echo)
	require.NoError(t, err)

	require.NoError(t, e.svc.SignalExec(ctx, "m", "entrypoint", 1))
	assert.Equal(t, []int32{1}, e.guest.Signals("entrypoint"))
	requireKind(t, e.svc.SignalExec(ctx, "m", "entrypoint", 0), machine.ErrInvalid)

	// Controls: another identifier the daemon holds no record of is still not
	// found, and the entrypoint needs a Running Machine.
	_, err = e.guest.Exec(ctx, "m", "unrecorded", echo)
	require.NoError(t, err)
	requireKind(t, e.svc.SignalExec(ctx, "m", "unrecorded", 1), machine.ErrNotFound)
	assert.Empty(t, e.guest.Signals("unrecorded"))
	refusedUnlessRunning(t, func(e *env) error {
		return e.svc.SignalExec(ctx, "m", "entrypoint", 1)
	})
}

// Row "Signal", columns On success, Repeat and After failure.
func TestSignalExec_OutcomeRepeatAndFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	_, err := e.svc.Exec(ctx, "m", "x", "r", echo)
	require.NoError(t, err)

	require.NoError(t, e.svc.SignalExec(ctx, "m", "x", 15))
	require.NoError(t, e.svc.SignalExec(ctx, "m", "x", 2), "a repeat delivers again")
	assert.Equal(t, []int32{15, 2}, e.guest.Signals("x"))
	e.requireState(t, "m", machine.StateRunning)

	e.guest.Finish("x", 0)
	requireKind(t, e.svc.SignalExec(ctx, "m", "x", 15), machine.ErrPreconditionFailed)
	assert.Equal(t, []int32{15, 2}, e.guest.Signals("x"), "an ended command takes nothing")

	e.guest.FailAt("signal", errors.New("boom"))
	_, err = e.svc.Exec(ctx, "m", "y", "r2", echo)
	require.NoError(t, err)
	requireKind(t, e.svc.SignalExec(ctx, "m", "y", 15), machine.ErrFailed)
	e.requireState(t, "m", machine.StateRunning)
}
