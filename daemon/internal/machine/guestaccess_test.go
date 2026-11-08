package machine_test

import (
	"bytes"
	"context"
	"errors"
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

	t.Run("a command that could not be started says so, and repeats so", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("exec", errBoom)

		record, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		assert.Equal(t, machine.ExecNotStarted, record.Status)
		assert.NotEmpty(t, record.Reason)
		e.requireState(t, "m", machine.StateRunning)

		e.guest.Heal("exec")
		again, err := e.svc.Exec(ctx, "m", "x", "r", echo)
		requireKind(t, err, machine.ErrFailed)
		assert.Equal(t, record, again, "the original outcome; mvmd never retries a command by itself")
		assert.Zero(t, e.guest.Started("x"))

		// Control: another exec identifier starts once the guest answers.
		_, err = e.svc.Exec(ctx, "m", "y", "r2", echo)
		require.NoError(t, err)
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
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.store, e.permit, machine.UIDRange{First: 1000, Count: 256}, e.events)
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

// Row "List and search".
func TestSearchFiles(t *testing.T) {
	ctx := context.Background()
	refusedUnlessRunning(t, func(e *env) error {
		_, err := e.svc.SearchFiles(ctx, "m", "/", "log")
		return err
	})

	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.guest.PutFile("m", "/var/a.log", "")
	e.guest.PutFile("m", "/var/b.txt", "")
	e.guest.PutFile("m", "/etc/c.log", "")

	got, err := e.svc.SearchFiles(ctx, "m", "/var/", "log")
	require.NoError(t, err)
	assert.Equal(t, []string{"/var/a.log"}, got)
	all, err := e.svc.SearchFiles(ctx, "m", "/", "log")
	require.NoError(t, err)
	assert.Equal(t, []string{"/etc/c.log", "/var/a.log"}, all, "control: a wider path finds more")

	e.guest.FailAt("search-files", errors.New("boom"))
	_, err = e.svc.SearchFiles(ctx, "m", "/", "log")
	requireKind(t, err, machine.ErrFailed)
}
