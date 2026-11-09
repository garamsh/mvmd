package machine_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// serveEcho makes the console of the Machine answer a banner and then echo its
// input in capitals, as a guest with a shell on its console would.
func serveEcho(e *env, id machine.MachineID, banner string) {
	e.driver.ServeConsole(id, func(c io.ReadWriteCloser) {
		defer c.Close()
		if banner != "" { // an empty write to a pipe would answer a read with no bytes
			_, _ = c.Write([]byte(banner))
		}
		buf := make([]byte, 64)
		for {
			n, err := c.Read(buf)
			if n > 0 {
				out := append([]byte(nil), buf[:n]...)
				for i := range out {
					if out[i] >= 'a' && out[i] <= 'z' {
						out[i] -= 'a' - 'A'
					}
				}
				_, _ = c.Write(out)
			}
			if err != nil {
				return
			}
		}
	})
}

func readN(t *testing.T, r io.Reader, n int) string {
	t.Helper()
	buf := make([]byte, n)
	_, err := io.ReadFull(r, buf)
	require.NoError(t, err)
	return string(buf)
}

// Row "Console", column Needs.
func TestConsole_Needs(t *testing.T) {
	ctx := context.Background()
	for _, from := range []machine.State{machine.StateCreated, machine.StateStopped, machine.StateFaulted} {
		t.Run("refused when "+from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			serveEcho(e, "m", "x")
			_, err := e.svc.Console(ctx, "m")
			requireKind(t, err, machine.ErrPreconditionFailed)
			e.requireState(t, "m", from)
		})
	}
	for _, from := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run("served when "+from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			serveEcho(e, "m", "hello")
			s, err := e.svc.Console(ctx, "m")
			require.NoError(t, err)
			defer s.Close()
			assert.Equal(t, "hello", readN(t, s, 5))
		})
	}
	t.Run("an absent Machine is not found", func(t *testing.T) {
		_, err := newEnv().svc.Console(ctx, "m")
		requireKind(t, err, machine.ErrNotFound)
	})
}

// Row "Console", column On success: a session that reads the console's output
// and writes its input.
func TestConsole_OnSuccessASessionReadsAndWrites(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	serveEcho(e, "m", "login: ")

	s, err := e.svc.Console(context.Background(), "m")
	require.NoError(t, err)
	defer s.Close()
	assert.Equal(t, "login: ", readN(t, s, 7))
	_, err = s.Write([]byte("root\n"))
	require.NoError(t, err)
	assert.Equal(t, "ROOT\n", readN(t, s, 5), "the input reached the guest's console")
}

// Row "Console", column Repeat: another session, which does not change the
// Machine and is not affected by the first.
func TestConsole_RepeatIsAnotherSession(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	serveEcho(e, "m", "hi")
	before := e.requireState(t, "m", machine.StateRunning)

	first, err := e.svc.Console(ctx, "m")
	require.NoError(t, err)
	second, err := e.svc.Console(ctx, "m")
	require.NoError(t, err)
	defer second.Close()
	assert.Equal(t, "hi", readN(t, first, 2))
	assert.Equal(t, "hi", readN(t, second, 2))

	require.NoError(t, first.Close())
	_, err = second.Write([]byte("a"))
	require.NoError(t, err, "closing one session leaves the other open")
	assert.Equal(t, "A", readN(t, second, 1))
	assert.Equal(t, before, e.requireState(t, "m", machine.StateRunning), "no state changed")
}

// Row "Console", column After failure: the Machine is unaffected.
func TestConsole_AfterFailureTheMachineIsUnaffected(t *testing.T) {
	ctx := context.Background()
	t.Run("the Driver cannot open the console", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		serveEcho(e, "m", "x")
		before := e.requireState(t, "m", machine.StateRunning)
		e.driver.FailAt("console", errBoom)

		_, err := e.svc.Console(ctx, "m")
		requireKind(t, err, machine.ErrFailed)
		assert.Equal(t, before, e.requireState(t, "m", machine.StateRunning))

		e.driver.Heal("console")
		s, err := e.svc.Console(ctx, "m")
		require.NoError(t, err, "a later session is opened")
		_ = s.Close()
	})
	t.Run("a console that is gone between the state check and the Driver is precondition failed", func(t *testing.T) {
		for _, from := range []machine.State{machine.StateRunning, machine.StatePaused} {
			e := newEnv()
			e.inState(t, "m", from) // recorded as it is; the Driver serves no console for it
			_, err := e.svc.Console(ctx, "m")
			requireKind(t, err, machine.ErrPreconditionFailed)
			assert.NotErrorIs(t, err, machine.ErrFailed)
			assert.Contains(t, err.Error(), from.String(), "the reason names the state")
			e.requireState(t, "m", from)
		}
	})
	t.Run("the same, injected as the Driver's own error", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		serveEcho(e, "m", "x")
		e.driver.FailAt("console", machine.ErrConsoleUnavailable)
		_, err := e.svc.Console(ctx, "m")
		requireKind(t, err, machine.ErrPreconditionFailed)
	})
	t.Run("the guest ends its console", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.driver.ServeConsole("m", func(c io.ReadWriteCloser) { _ = c.Close() })
		s, err := e.svc.Console(ctx, "m")
		require.NoError(t, err)
		defer s.Close()
		_, err = s.Read(make([]byte, 1))
		assert.Error(t, err, "the session ends")
		e.requireState(t, "m", machine.StateRunning)
	})
}

// The session is a read path and does not detach from the caller: it ends with
// the caller's context. Nor does it hold the Machine's lock.
func TestConsole_EndsWithTheCallersContextAndHoldsNoLock(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	serveEcho(e, "m", "")

	ctx, cancel := context.WithCancel(context.Background())
	s, err := e.svc.Console(ctx, "m")
	require.NoError(t, err)

	pause := make(chan error, 1)
	go func() { pause <- e.svc.Pause(context.Background(), "m") }()
	select {
	case err := <-pause:
		require.NoError(t, err, "a lifecycle call is not held up by an open session")
	case <-time.After(5 * time.Second):
		t.Fatal("Pause waited for the console session")
	}

	done := make(chan error, 1)
	go func() { _, err := s.Read(make([]byte, 1)); done <- err }()
	cancel()
	select {
	case err := <-done:
		assert.Error(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the session outlived its caller's context")
	}
}
