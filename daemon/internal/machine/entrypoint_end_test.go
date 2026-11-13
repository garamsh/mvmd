package machine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const agent = "mvmd-guest-agent: "

// endedAt is the time the console lines of the exit cases name.
var endedAt = time.Date(2026, 11, 11, 12, 34, 56, 123456789, time.UTC)

// TestReadExecOutput_EntrypointEndFromTheConsoleLog parses each line form that
// guest.md lists, through the read of the entrypoint's record of a Stopped
// Machine, whose guest cannot be asked.
func TestReadExecOutput_EntrypointEndFromTheConsoleLog(t *testing.T) {
	tests := []struct {
		name string
		log  string
		want machine.ExecRecord
	}{
		{"exited with a code", agent + "entrypoint exited code=3\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 3}},
		{"exited with code zero", agent + "entrypoint exited code=0\n",
			machine.ExecRecord{Status: machine.ExecExited}},
		{"exited by a signal", agent + "entrypoint exited signal=9\n",
			machine.ExecRecord{Status: machine.ExecExited, TermSignal: 9}},
		{"exited with a code at a time", agent + "entrypoint exited code=3 at=2026-11-11T12:34:56.123456789Z\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 3, EndedAt: endedAt}},
		{"exited by a signal at a time", agent + "entrypoint exited signal=9 at=2026-11-11T12:34:56.123456789Z\n",
			machine.ExecRecord{Status: machine.ExecExited, TermSignal: 9, EndedAt: endedAt}},
		{"a time with a 3-digit fraction", agent + "entrypoint exited code=3 at=2026-11-11T12:34:56.123Z\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 3, EndedAt: endedAt.Truncate(time.Millisecond)}},
		{"a time with no fraction", agent + "entrypoint exited signal=9 at=2026-11-11T12:34:56Z\n",
			machine.ExecRecord{Status: machine.ExecExited, TermSignal: 9, EndedAt: endedAt.Truncate(time.Second)}},
		{"a time with an offset is the same instant in UTC", agent + "entrypoint exited code=3 at=2026-11-11T21:34:56.123456789+09:00\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 3, EndedAt: endedAt}},
		{"a code at a time that is not a time keeps the code and no time", agent + "entrypoint exited code=3 at=yesterday\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 3}},
		{"a signal at an empty time keeps the signal and no time", agent + "entrypoint exited signal=9 at=\n",
			machine.ExecRecord{Status: machine.ExecExited, TermSignal: 9}},
		{"a code that is not a number is not a line, whatever time follows", agent + "entrypoint exited code=abc at=2026-11-11T12:34:56.123456789Z\n",
			machine.ExecRecord{Status: machine.ExecCutShort}},
		{"not started", agent + "entrypoint not started reason=no such file: /bin/app\n",
			machine.ExecRecord{Status: machine.ExecNotStarted, Reason: "no such file: /bin/app"}},
		{"ended, cut short", agent + "entrypoint ended reason=the output could not be kept\n",
			machine.ExecRecord{Status: machine.ExecCutShort, Reason: "the output could not be kept"}},
		{"terminated before the entrypoint ran", agent + "terminated before the entrypoint ran\n",
			machine.ExecRecord{Status: machine.ExecNotStarted, Reason: "the guest was terminated before the entrypoint ran"}},
		{"boot failed", agent + "boot failed step=mount the root disk detail=no such device\n",
			machine.ExecRecord{Status: machine.ExecNotStarted, Reason: "the guest's boot failed at mount the root disk: no such device"}},
		{"the agent failed", agent + "failed to power off: EPERM\n",
			machine.ExecRecord{Status: machine.ExecCutShort, Reason: "the Guest Agent ended: failed to power off: EPERM"}},
		{"the agent panicked", agent + "panic: index out of bounds at main.rs:10:5\n",
			machine.ExecRecord{Status: machine.ExecCutShort, Reason: "the Guest Agent ended: panic: index out of bounds at main.rs:10:5"}},
		{"among the kernel's lines, with carriage returns",
			"[    0.1] Linux version\r\n[    1.2] eth0 up\r\n" + agent + "entrypoint exited code=7\r\n[    2.0] reboot: Power down\r\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 7}},
		{"the last line has no line end", agent + "entrypoint exited code=4",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 4}},
		{"the entrypoint's end decides over a later failure of the agent",
			agent + "entrypoint exited code=1\n" + agent + "failed to power off: EPERM\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 1}},
		{"the entrypoint's end decides over an earlier failure of the agent",
			agent + "failed to listen: busy\n" + agent + "entrypoint exited code=1\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 1}},
		{"the first of two as informed",
			agent + "entrypoint exited code=1\n" + agent + "entrypoint exited code=2\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 1}},
		{"a line the agent did not write is not the agent's",
			"entrypoint exited code=5\n[    1] " + agent + "entrypoint exited code=6\n",
			machine.ExecRecord{Status: machine.ExecCutShort}},
		{"an exit code that is not a number is not a line",
			agent + "entrypoint exited code=abc\n",
			machine.ExecRecord{Status: machine.ExecCutShort}},
		{"an overlong line is skipped, and the next is read",
			agent + "entrypoint not started reason=" + strings.Repeat("x", 200<<10) + "\n" + agent + "entrypoint exited code=2\n",
			machine.ExecRecord{Status: machine.ExecExited, ExitCode: 2}},
		{"no line speaks of it", "[    1] booted\n",
			machine.ExecRecord{Status: machine.ExecCutShort}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", machine.StateStopped)
			e.driver.WriteConsoleLog("m", tt.log)

			record, err := e.svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, 0,
				func(machine.OutputChunk) error { t.Fatal("no output comes from the log"); return nil })
			require.NoError(t, err)
			tt.want.ID = "entrypoint"
			if tt.want.Status == machine.ExecCutShort && tt.want.Reason == "" {
				assert.NotEmpty(t, record.Reason, "a Machine that ended without a line is cut short, with the reason")
				tt.want.Reason = record.Reason
			}
			assert.Equal(t, tt.want, record)
		})
	}
}

func TestReadExecOutput_EntrypointFromTheLogOfAFaultedMachine(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateFaulted)
	e.driver.WriteConsoleLog("m", agent+"entrypoint exited code=9\n")
	record, err := e.svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, 0, nil)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecRecord{ID: "entrypoint", Status: machine.ExecExited, ExitCode: 9}, record)
}

func TestReadExecOutput_EntrypointLogCannotBeRead(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateStopped) // the Driver has no log for it
	_, err := e.svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, 0, nil)
	requireKind(t, err, machine.ErrFailed)
}

// While the guest answers, it holds the record, and the log is not read.
func TestReadExecOutput_EntrypointOfARunningMachineIsTheGuests(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.driver.WriteConsoleLog("m", agent+"entrypoint exited code=1\n")
	_, err := e.guest.Exec(ctx, "m", "entrypoint", machine.ExecSpec{})
	require.NoError(t, err)
	e.guest.Emit("entrypoint", machine.OutputStdout, "out")
	e.guest.Finish("entrypoint", 5)

	var got []machine.OutputChunk
	record, err := e.svc.ReadExecOutput(ctx, "m", "entrypoint", 0, 0, func(c machine.OutputChunk) error {
		got = append(got, c)
		return nil
	})
	require.NoError(t, err)
	assert.Equal(t, machine.ExecRecord{ID: "entrypoint", Status: machine.ExecExited, ExitCode: 5}, record)
	require.Len(t, got, 1)
	assert.Equal(t, "out", string(got[0].Data))
}

// The agent powers the Machine off after the entrypoint ends, so a Machine
// still Running may have a guest that no longer answers; the log has the end.
func TestReadExecOutput_EntrypointOfARunningMachineWhoseGuestIsGone(t *testing.T) {
	ctx := context.Background()
	t.Run("the log has the end", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("read-exec-output", errBoom)
		e.driver.WriteConsoleLog("m", agent+"entrypoint exited code=8\n")
		record, err := e.svc.ReadExecOutput(ctx, "m", "entrypoint", 0, 0, nil)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecRecord{ID: "entrypoint", Status: machine.ExecExited, ExitCode: 8}, record)
	})
	t.Run("the log has nothing, so the guest's failure is the answer", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("read-exec-output", errBoom)
		e.driver.WriteConsoleLog("m", "[    1] booted\n")
		_, err := e.svc.ReadExecOutput(ctx, "m", "entrypoint", 0, 0, nil)
		requireKind(t, err, machine.ErrFailed)
	})
	t.Run("there is no log either", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("read-exec-output", errBoom)
		_, err := e.svc.ReadExecOutput(ctx, "m", "entrypoint", 0, 0, nil)
		requireKind(t, err, machine.ErrFailed)
	})
}

func TestReadExecOutput_EntrypointNeedsRunningOrEnded(t *testing.T) {
	for _, from := range []machine.State{machine.StateCreated, machine.StatePaused} {
		t.Run(from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			e.driver.WriteConsoleLog("m", agent+"entrypoint exited code=1\n")
			_, err := e.svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, 0, nil)
			requireKind(t, err, machine.ErrPreconditionFailed)
		})
	}
	t.Run("an absent Machine", func(t *testing.T) {
		_, err := newEnv().svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, 0, nil)
		requireKind(t, err, machine.ErrNotFound)
	})
}
