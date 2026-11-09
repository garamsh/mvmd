package drivertest

import (
	"bytes"
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/supervisor"
)

// ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect boots a Machine
// and reaches its Guest Agent through GuestConn; opens sessions to its console,
// if the Driver reports the console capability, and asserts that the Driver
// refuses a session if it does not; pauses and resumes it; and ends it.
func ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t *testing.T, s Subject) {
	// The directory's path is over what a Unix socket address holds: the VMM
	// binds its sockets at short paths in its root, and the Driver reaches them
	// through a descriptor of the run directory.
	state := longTempDir(t, "dtboot")
	run := statedir.Run(state)
	require.NoError(t, os.Mkdir(run, 0o700))
	d := s.Driver(t, RunDirs{"m1": run})

	devices := []machine.BlockDevice{diskFile(t, state, "image", true), diskFile(t, state, "root", false)}
	spec := machine.BootSpec{Devices: devices, Resources: resources, RunDir: run}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)

	// What the supervisor does, without its confinement: the described process
	// runs with its standard input and output on the console terminal the
	// supervisor opens and serves in the state directory (supervisor.OpenConsole),
	// and the run directory's log.
	logPath := statedir.VMMLog(run)
	vmmLog, err := os.Create(logPath)
	require.NoError(t, err)
	defer vmmLog.Close()
	console, vmmIn, vmmOut, err := supervisor.OpenConsole(state, vmmLog)
	require.NoError(t, err)
	t.Cleanup(func() { _ = console.Close() })
	proc := startVMM(t, s, p.For(spec), Streams{In: vmmIn, Out: vmmOut, Err: vmmLog}, logPath)
	require.NoError(t, vmmIn.Close())
	require.NoError(t, vmmOut.Close())

	require.NoError(t, d.Boot(ctx, "m1", spec))

	// The Guest Agent starts listening some time after the kernel boots, so a
	// call retries until it answers. Its answer is what proves the whole path:
	// the Driver's connection to the guest and the agent.
	guest := s.Guest(d)
	require.Eventually(t, func() bool { return healthy(ctx, guest, "m1", 5*time.Second) },
		time.Minute, 500*time.Millisecond, "the Guest Agent never answered Health through GuestConn")

	var session io.ReadWriteCloser
	if has(d, machine.CapabilityConsole) {
		session = consoleSessions(ctx, t, d)
	} else {
		_, err := d.Console(ctx, "m1")
		assert.ErrorIs(t, err, machine.ErrUnsupported, "a Driver that reports no console capability refuses a session")
	}

	// A paused guest runs nothing, so the agent answers nothing until resumed.
	require.NoError(t, d.Pause(ctx, "m1"))
	assert.False(t, healthy(ctx, guest, "m1", 2*time.Second), "a paused guest does not answer")
	require.NoError(t, d.Resume(ctx, "m1"))
	require.Eventually(t, func() bool { return healthy(ctx, guest, "m1", 5*time.Second) },
		30*time.Second, 200*time.Millisecond, "the guest did not answer after Resume")

	// Stop: Shutdown asks, and what the guest's kernel does with it is the
	// kernel's. The VMM is ended either way, as the supervisor ends it.
	if err := d.Shutdown(ctx, "m1", 2*time.Second); err != nil {
		t.Logf("the guest did not stop gracefully: %v", err)
	}
	require.NoError(t, proc.Kill())
	select {
	case <-proc.Exited():
	case <-time.After(10 * time.Second):
		t.Fatal("the VMM did not end after being killed")
	}
	_, err = d.GuestConn(ctx, "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable, "no guest is reachable once the VMM has ended")

	// The VMM's output has ended, so an open session reads to its end and no
	// more open.
	console.Wait()
	if session != nil {
		defer session.Close()
		_, err = io.Copy(io.Discard, session)
		require.NoError(t, err, "the session ends with end of file when the VMM ends")
		_, err = d.Console(ctx, "m1")
		require.ErrorIs(t, err, machine.ErrConsoleUnavailable)
	}
}

// consoleSessions is the console half of the scenario: what a session writes is
// the guest's serial input and the guest's kernel answers it, a second session
// beside the first reads the guest's answer too, and the console log holds the
// boot output. It returns the first session, still open.
func consoleSessions(ctx context.Context, t *testing.T, d machine.Driver) io.ReadWriteCloser {
	t.Helper()
	session, err := d.Console(ctx, "m1")
	require.NoError(t, err)
	_, err = session.Write([]byte("mvmd-console-probe\n"))
	require.NoError(t, err)
	require.NoError(t, awaitOutput(session, "mvmd-console-probe", 30*time.Second), "the guest did not answer on the console")

	// The first is unaffected when the second closes. The log holds the boot
	// output, which no session was open for, and what the sessions caused.
	other, err := d.Console(ctx, "m1")
	require.NoError(t, err)
	_, err = other.Write([]byte("mvmd-second-probe\n"))
	require.NoError(t, err)
	require.NoError(t, awaitOutput(other, "mvmd-second-probe", 30*time.Second))
	require.NoError(t, other.Close())
	_, err = session.Write([]byte("mvmd-after-close\n"))
	require.NoError(t, err)
	require.NoError(t, awaitOutput(session, "mvmd-after-close", 30*time.Second), "closing one session leaves the other working")
	require.Eventually(t, func() bool {
		log, err := d.ConsoleLog(ctx, "m1")
		if err != nil {
			return false
		}
		raw, _ := io.ReadAll(log)
		_ = log.Close()
		return bytes.Contains(raw, []byte("Linux version")) && bytes.Contains(raw, []byte("mvmd-console-probe"))
	}, 10*time.Second, 100*time.Millisecond, "the console log holds the boot output and the session's echo")
	return session
}
