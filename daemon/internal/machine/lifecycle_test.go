package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Row "Start", column Needs.
func TestStart_Needs(t *testing.T) {
	ctx := context.Background()

	t.Run("Created and Stopped start; other states and absent Machines do not", func(t *testing.T) {
		tests := []struct {
			from machine.State
			kind error
		}{
			{machine.StateCreated, nil},
			{machine.StateStopped, nil},
			{machine.StatePaused, machine.ErrPreconditionFailed},
			{machine.StateFaulted, machine.ErrPreconditionFailed},
		}
		for _, tt := range tests {
			t.Run(tt.from.String(), func(t *testing.T) {
				e := newEnv()
				e.inState(t, "m", tt.from)
				starts := e.sup.Calls("start")

				err := e.svc.Start(ctx, "m")
				if tt.kind == nil {
					require.NoError(t, err)
					e.requireState(t, "m", machine.StateRunning)
					return
				}
				requireKind(t, err, tt.kind)
				e.requireState(t, "m", tt.from)
				assert.Equal(t, starts, e.sup.Calls("start"), "nothing started")
			})
		}
		requireKind(t, newEnv().svc.Start(ctx, "absent"), machine.ErrNotFound)
	})

	t.Run("the fencing port must permit the start", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.permit.FailAt("permit", errBoom)

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrPreconditionFailed)
		e.requireState(t, "m", machine.StateCreated)
		assert.Zero(t, e.sup.Calls("start"), "no VMM process started")
		assert.Zero(t, e.net.Calls("apply"), "refused before anything else")

		// Control: permitted, the same Machine starts.
		e.permit.Heal("permit")
		require.NoError(t, e.svc.Start(ctx, "m"))
	})

	t.Run("the network policy must be applied first", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.net.FailAt("apply", errBoom)

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrPreconditionFailed)
		e.requireState(t, "m", machine.StateCreated)
		assert.Zero(t, e.sup.Calls("start"), "no VMM process started")
		assert.Zero(t, e.driver.Calls("boot"), "no guest booted")
		_, running := e.sup.Running("m")
		assert.False(t, running)

		// Control: with the policy applied, the same Machine starts.
		e.net.Heal("apply")
		require.NoError(t, e.svc.Start(ctx, "m"))
		applied, ok := e.net.Applied("m")
		require.True(t, ok)
		assert.Equal(t, baseSpec().Policy, applied, "the Machine's own policy was applied")
	})

	t.Run("the image and disks must still be on the node", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.images.FailAt("locate", machine.ErrImageAbsent)
		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrPreconditionFailed)
		e.requireState(t, "m", machine.StateCreated)
		assert.Zero(t, e.sup.Calls("start"))
	})
}

// Row "Start", column On success.
func TestStart_OnSuccess(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.disks.Provide("data")
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{{Disk: "data", ReadOnly: true}}
	require.NoError(t, e.svc.Create(ctx, "m", "r", spec))
	e.permit.FailAt("permit", nil) // counts the call without failing it

	require.NoError(t, e.svc.Start(ctx, "m"))
	m := e.requireState(t, "m", machine.StateRunning)
	e.requireStored(t, "m", machine.StateRunning)
	assert.Equal(t, 1, e.permit.Calls("permit"))

	// The Driver was given the devices in boot order and the Machine's tap.
	boot := e.driver.BootSpec("m")
	assert.Equal(t, []machine.BlockDevice{
		{Path: "/images/sha256:image.erofs", ReadOnly: true},
		{Disk: m.RootDisk, Path: "/disks/m-root"},
		{Disk: "data", Path: "/disks/data", ReadOnly: true},
	}, boot.Devices)
	assert.Equal(t, machine.NetworkDevice{Tap: "tap-m", MAC: m.Network.MAC}, boot.Network)
	assert.Equal(t, spec.Resources, boot.Resources)

	// mvmd started the process the Driver described, under the supervisor.
	process, running := e.sup.Running("m")
	require.True(t, running)
	assert.Equal(t, boot.Devices, process.BootFiles)
	assert.Equal(t, 1, e.driver.Calls("boot"))
}

// Row "Start", column Repeat.
func TestStart_Repeat(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.permit.FailAt("permit", errBoom)

	require.NoError(t, e.svc.Start(context.Background(), "m"), "Running stays success even when starting is no longer permitted")
	assert.Equal(t, 1, e.sup.Calls("start"))
	assert.Equal(t, 1, e.driver.Calls("boot"))
	e.requireState(t, "m", machine.StateRunning)
}

// Row "Start", column State after failure.
func TestStart_StateAfterFailure(t *testing.T) {
	ctx := context.Background()
	for _, from := range []machine.State{machine.StateCreated, machine.StateStopped} {
		t.Run("from "+from.String()+": before the boot began the prior state stays", func(t *testing.T) {
			for _, step := range []string{"describe", "start"} {
				e := newEnv()
				e.inState(t, "m", from)
				if step == "describe" {
					e.driver.FailAt("describe", errBoom)
				} else {
					e.sup.FailAt("start", errBoom)
				}
				before := e.driver.Calls("boot")

				requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
				e.requireState(t, "m", from)
				e.requireStored(t, "m", from)
				assert.Equal(t, before, e.driver.Calls("boot"), "the guest never booted")
				_, running := e.sup.Running("m")
				assert.False(t, running)
			}
		})

		t.Run("from "+from.String()+": a boot that began and failed ends Stopped with the reason", func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			e.driver.FailAt("boot", errBoom)

			err := e.svc.Start(ctx, "m")
			requireKind(t, err, machine.ErrFailed)
			m := e.requireState(t, "m", machine.StateStopped)
			assert.Contains(t, m.FailureReason, "injected failure", "Inspect reports the reason for the last failure")
			_, running := e.sup.Running("m")
			assert.False(t, running, "the VMM process was ended")

			// Control: the same Machine boots once the Driver stops failing.
			e.driver.Heal("boot")
			require.NoError(t, e.svc.Start(ctx, "m"))
			assert.Empty(t, e.requireState(t, "m", machine.StateRunning).FailureReason)
		})
	}

	t.Run("a failed boot whose VMM cannot be ended is Faulted", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.driver.FailAt("boot", errBoom)
		e.sup.FailAt("terminate", errBoom)

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
		e.requireStored(t, "m", machine.StateFaulted)
	})

	t.Run("a start that cannot be recorded is Faulted", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.store.FailAt("save", errBoom)

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
	})
}

// Row "Stop", column Needs.
func TestStop_Needs(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		from, to machine.State
	}{
		{machine.StateCreated, machine.StateCreated},
		{machine.StateRunning, machine.StateStopped},
		{machine.StatePaused, machine.StateStopped},
		{machine.StateStopped, machine.StateStopped},
		{machine.StateFaulted, machine.StateStopped},
	}
	for _, tt := range tests {
		t.Run("from "+tt.from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", tt.from)
			require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
			e.requireState(t, "m", tt.to)
		})
	}
	requireKind(t, newEnv().svc.Stop(ctx, "absent", time.Second), machine.ErrNotFound)
}

// Row "Stop", column On success.
func TestStop_OnSuccess(t *testing.T) {
	ctx := context.Background()

	t.Run("asks the guest for the grace period and ends the VMM", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)

		require.NoError(t, e.svc.Stop(ctx, "m", 7*time.Second))
		e.requireState(t, "m", machine.StateStopped)
		e.requireStored(t, "m", machine.StateStopped)
		assert.Equal(t, 7*time.Second, e.driver.Grace())
		_, running := e.sup.Running("m")
		assert.False(t, running, "a stop always ends with the VMM terminated")
	})

	t.Run("a guest that does not stop is forced down", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("shutdown", errBoom)

		require.NoError(t, e.svc.Stop(ctx, "m", time.Millisecond))
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running)
		assert.Equal(t, 1, e.sup.Calls("terminate"))
	})
}

// Row "Stop", column Repeat.
func TestStop_Repeat(t *testing.T) {
	for _, from := range []machine.State{machine.StateStopped, machine.StateCreated} {
		t.Run("already "+from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			shutdowns, terminates := e.driver.Calls("shutdown"), e.sup.Calls("terminate")

			require.NoError(t, e.svc.Stop(context.Background(), "m", time.Second))
			e.requireState(t, "m", from)
			assert.Equal(t, shutdowns, e.driver.Calls("shutdown"), "no change")
			assert.Equal(t, terminates, e.sup.Calls("terminate"), "no change")
		})
	}
}

// Row "Stop", column State after failure.
func TestStop_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a stop that never began leaves Running or Paused", func(t *testing.T) {
		for _, from := range []machine.State{machine.StateRunning, machine.StatePaused} {
			e := newEnv()
			e.inState(t, "m", from)
			e.sup.FailAt("terminate", machine.ErrNotAttempted)

			requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
			e.requireState(t, "m", from)
			e.requireStored(t, "m", from)
		}
	})

	t.Run("a stop that began and cannot be confirmed is Faulted", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.sup.FailAt("terminate", errBoom)

		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		m := e.requireState(t, "m", machine.StateFaulted)
		assert.Contains(t, m.FailureReason, "could not be confirmed")
		e.requireStored(t, "m", machine.StateFaulted)

		// A Faulted Machine is stopped once the VMM can be confirmed ended.
		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
		e.sup.Heal("terminate")
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
		e.requireState(t, "m", machine.StateStopped)
	})

	t.Run("a stop that cannot be recorded is Faulted", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.store.FailAt("save", errBoom)

		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
	})
}

// The Faulted state: only stop, delete, inspect and reads apply.
func TestFaulted_OnlyStopDeleteInspectAndReadsApply(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.disks.Provide("d")
	e.inState(t, "m", machine.StateFaulted)
	before := e.requireState(t, "m", machine.StateFaulted)

	calls := map[string]error{
		"start":         e.svc.Start(ctx, "m"),
		"pause":         e.svc.Pause(ctx, "m"),
		"resume":        e.svc.Resume(ctx, "m"),
		"attach disk":   e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "d"}),
		"detach disk":   e.svc.DetachDisk(ctx, "m", "d"),
		"set resources": e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}),
	}
	for name, err := range calls {
		t.Run(name+" is refused", func(t *testing.T) {
			requireKind(t, err, machine.ErrPreconditionFailed)
		})
	}
	assert.Equal(t, before, e.requireState(t, "m", machine.StateFaulted), "nothing changed")
	assert.Equal(t, 1, e.sup.Calls("start"), "no second start")
	assert.Zero(t, e.driver.Calls("pause")+e.driver.Calls("resume")+e.driver.Calls("attach")+e.driver.Calls("resize"))

	list, err := e.svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, list, 1, "reads apply")

	// Control: Delete applies.
	require.NoError(t, e.svc.Delete(ctx, "m"))
}

// Rows "Pause" and "Resume", column Needs.
func TestPauseAndResume_Needs(t *testing.T) {
	ctx := context.Background()
	for _, op := range pauseResume(t) {
		t.Run(op.name, func(t *testing.T) {
			for _, from := range []machine.State{machine.StateCreated, machine.StateRunning, machine.StatePaused, machine.StateStopped, machine.StateFaulted} {
				e := newEnv()
				e.inState(t, "m", from)
				err := op.call(e.svc, ctx, "m")
				if from == op.from || from == op.to {
					require.NoError(t, err, "from %s", from)
					continue
				}
				requireKind(t, err, machine.ErrPreconditionFailed)
				e.requireState(t, "m", from)
			}
			requireKind(t, op.call(newEnv().svc, ctx, "absent"), machine.ErrNotFound)
		})
	}
}

// Rows "Pause" and "Resume", column On success.
func TestPauseAndResume_OnSuccess(t *testing.T) {
	for _, op := range pauseResume(t) {
		t.Run(op.name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", op.from)
			require.NoError(t, op.call(e.svc, context.Background(), "m"))
			e.requireState(t, "m", op.to)
			e.requireStored(t, "m", op.to)
			assert.Equal(t, 1, e.driver.Calls(op.step))
		})
	}
}

// Rows "Pause" and "Resume", column Repeat.
func TestPauseAndResume_Repeat(t *testing.T) {
	for _, op := range pauseResume(t) {
		t.Run(op.name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", op.to)
			calls := e.driver.Calls(op.step)
			require.NoError(t, op.call(e.svc, context.Background(), "m"))
			e.requireState(t, "m", op.to)
			assert.Equal(t, calls, e.driver.Calls(op.step), "the Driver was not asked again")
		})
	}
}

// Rows "Pause" and "Resume", column State after failure.
func TestPauseAndResume_StateAfterFailure(t *testing.T) {
	for _, op := range pauseResume(t) {
		t.Run(op.name+" fails in the Driver", func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", op.from)
			e.driver.FailAt(op.step, errBoom)

			requireKind(t, op.call(e.svc, context.Background(), "m"), machine.ErrFailed)
			e.requireState(t, "m", op.from)
			e.requireStored(t, "m", op.from)
		})
		t.Run(op.name+" cannot be recorded", func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", op.from)
			e.store.FailAt("save", errBoom)

			requireKind(t, op.call(e.svc, context.Background(), "m"), machine.ErrFailed)
			e.requireState(t, "m", machine.StateFaulted)
		})
	}
}

type moveOp struct {
	name, step string
	from, to   machine.State
	call       func(machine.Service, context.Context, machine.MachineID) error
}

func pauseResume(t *testing.T) []moveOp {
	t.Helper()
	return []moveOp{
		{"Pause", "pause", machine.StateRunning, machine.StatePaused, machine.Service.Pause},
		{"Resume", "resume", machine.StatePaused, machine.StateRunning, machine.Service.Resume},
	}
}

// Row "Delete Machine", column Needs.
func TestDelete_Needs(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		from machine.State
		kind error
	}{
		{machine.StateCreated, nil},
		{machine.StateStopped, nil},
		{machine.StateFaulted, nil},
		{machine.StateRunning, machine.ErrPreconditionFailed},
		{machine.StatePaused, machine.ErrPreconditionFailed},
	}
	for _, tt := range tests {
		t.Run(tt.from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", tt.from)
			err := e.svc.Delete(ctx, "m")
			if tt.kind == nil {
				require.NoError(t, err)
				e.requireNoMachine(t, "m")
				return
			}
			requireKind(t, err, tt.kind)
			e.requireState(t, "m", tt.from)
			_, held := e.disks.Holds("m-root")
			assert.True(t, held, "the root disk is kept")
		})
	}
}

// Row "Delete Machine", column On success.
func TestDelete_OnSuccess(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.inState(t, "m", machine.StateFaulted)

	require.NoError(t, e.svc.Delete(ctx, "m"))
	e.requireNoMachine(t, "m")
	_, running := e.sup.Running("m")
	assert.False(t, running, "a Faulted Machine's VMM is ended before its disks are released")
	_, stored := e.store.Record("m")
	assert.False(t, stored)
	list, err := e.svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, list)

	// The identifier and its request ID are free again.
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), baseSpec()))
}

// Row "Delete Machine", column Repeat.
func TestDelete_Repeat(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.created(t, "m")
	require.NoError(t, e.svc.Delete(ctx, "m"))
	require.NoError(t, e.svc.Delete(ctx, "m"), "absent: success")
	require.NoError(t, newEnv().svc.Delete(ctx, "never-existed"))
}

// Row "Delete Machine", column State after failure.
func TestDelete_StateAfterFailure(t *testing.T) {
	ctx := context.Background()
	steps := []struct {
		name string
		fail func(*env)
		heal func(*env)
	}{
		{"releasing the network", func(e *env) { e.net.FailAt("release", errBoom) }, func(e *env) { e.net.Heal("release") }},
		{"removing the root disk", func(e *env) { e.disks.FailAt("remove", errBoom) }, func(e *env) { e.disks.Heal("remove") }},
		{"removing the record", func(e *env) { e.store.FailAt("remove", errBoom) }, func(e *env) { e.store.Heal("remove") }},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", machine.StateStopped)
			tt.fail(e)

			requireKind(t, e.svc.Delete(ctx, "m"), machine.ErrFailed)
			e.requireState(t, "m", machine.StateStopped)

			// Control: the same delete succeeds once the step stops failing.
			tt.heal(e)
			require.NoError(t, e.svc.Delete(ctx, "m"))
			e.requireNoMachine(t, "m")
		})
	}

	t.Run("a Faulted Machine whose VMM cannot be ended stays Faulted", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateFaulted)
		e.sup.FailAt("terminate", errBoom)

		requireKind(t, e.svc.Delete(ctx, "m"), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
		_, held := e.disks.Holds("m-root")
		assert.True(t, held, "the disk is released only once the VMM has ended")
	})
}

func TestInspectAndList_ReadWithoutChangingState(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.created(t, "b")
	e.created(t, "a")
	e.inState(t, "c", machine.StateRunning)
	requireKind(t, e.svc.Start(ctx, "absent"), machine.ErrNotFound)
	_, err := e.svc.Inspect(ctx, "absent")
	requireKind(t, err, machine.ErrNotFound)

	first, err := e.svc.List(ctx)
	require.NoError(t, err)
	second, err := e.svc.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, first, second)
	var ids []machine.MachineID
	for _, m := range first {
		ids = append(ids, m.ID)
	}
	assert.Equal(t, []machine.MachineID{"a", "b", "c"}, ids)
	assert.Equal(t, machine.StateRunning, first[2].State)
}

// ADR 0011: the tap exists while the VMM process exists, and goes after it.
func TestNetwork_RemovedWhenNoVMMHoldsIt(t *testing.T) {
	ctx := context.Background()
	requireNoTap := func(t *testing.T, e *env) {
		t.Helper()
		_, applied := e.net.Applied("m")
		assert.False(t, applied, "the Network port still holds a tap for the Machine")
		assert.True(t, e.net.Allocated("m"), "the addresses are the Machine's until delete")
	}

	failures := []struct {
		name string
		fail func(*env)
	}{
		{"the Driver cannot describe the process", func(e *env) { e.driver.FailAt("describe", errBoom) }},
		{"the supervisor cannot start the process", func(e *env) { e.sup.FailAt("start", errBoom) }},
		{"the boot fails", func(e *env) { e.driver.FailAt("boot", errBoom) }},
	}
	for _, tt := range failures {
		t.Run("a start fails when "+tt.name, func(t *testing.T) {
			e := newEnv()
			e.created(t, "m")
			tt.fail(e)
			requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
			assert.Equal(t, 1, e.net.Calls("apply"), "the policy was applied before the failure")
			requireNoTap(t, e)
		})
	}

	t.Run("a start that succeeds keeps the tap", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		_, applied := e.net.Applied("m")
		assert.True(t, applied)
	})

	t.Run("a stop removes the tap once the VMM has ended", func(t *testing.T) {
		for _, from := range []machine.State{machine.StateRunning, machine.StatePaused, machine.StateFaulted} {
			e := newEnv()
			e.inState(t, "m", from)
			require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
			requireNoTap(t, e)
		}
	})

	t.Run("a stop that never began keeps the tap with the running VMM", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.sup.FailAt("terminate", machine.ErrNotAttempted)
		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		_, applied := e.net.Applied("m")
		assert.True(t, applied)
	})

	t.Run("a tap that cannot be removed after the VMM ended leaves Faulted, and a stop retries it", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.net.FailAt("withdraw", errBoom)
		requireKind(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)

		e.net.Heal("withdraw")
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
		e.requireState(t, "m", machine.StateStopped)
		requireNoTap(t, e)
	})

	t.Run("a failed boot whose tap cannot be removed is Faulted", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.driver.FailAt("boot", errBoom)
		e.net.FailAt("withdraw", errBoom)
		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
	})

	t.Run("a start restarts the tap after a stop", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateStopped)
		require.NoError(t, e.svc.Start(ctx, "m"))
		_, applied := e.net.Applied("m")
		assert.True(t, applied)
	})
}
