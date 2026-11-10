package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var allCaps = []machine.Capability{machine.CapabilitySnapshot, machine.CapabilityDiskHotplug, machine.CapabilityResize}

// acceptedCall is one state-changing call, with the state it starts from, the
// step of a substitute it is held at, and what it must reach.
type acceptedCall struct {
	name    string
	arrange func(t *testing.T, e *env)
	// hold makes the next call of a step wait, and returns the way to know it
	// does and to let it go on.
	hold func(e *env) (entered <-chan struct{}, release func())
	call func(ctx context.Context, svc machine.Service) error
	// reached requires the state the call's success names.
	reached func(t *testing.T, e *env, svc machine.Service)
}

func holdStore(step string) func(e *env) (<-chan struct{}, func()) {
	return func(e *env) (<-chan struct{}, func()) { return e.store.Hold(step) }
}

func acceptedCalls() []acceptedCall {
	state := func(id machine.MachineID, want machine.State) func(*testing.T, *env, machine.Service) {
		return func(t *testing.T, _ *env, svc machine.Service) {
			m, err := svc.Inspect(context.Background(), id)
			require.NoError(t, err)
			require.Equal(t, want, m.State)
		}
	}
	inState := func(s machine.State) func(*testing.T, *env) {
		return func(t *testing.T, e *env) { e.inState(t, "m", s) }
	}
	return []acceptedCall{
		{
			name: "Create", arrange: func(*testing.T, *env) {}, hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.Create(ctx, "m", "req-m", baseSpec())
			},
			reached: state("m", machine.StateCreated),
		},
		{
			name: "Start", arrange: inState(machine.StateCreated), hold: holdStore("save"),
			call:    func(ctx context.Context, svc machine.Service) error { return svc.Start(ctx, "m") },
			reached: state("m", machine.StateRunning),
		},
		{
			name: "Stop", arrange: inState(machine.StateRunning), hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.Stop(ctx, "m", time.Second)
			},
			reached: state("m", machine.StateStopped),
		},
		{
			name: "ForceStop", arrange: inState(machine.StateRunning), hold: holdStore("save"),
			call:    func(ctx context.Context, svc machine.Service) error { return svc.ForceStop(ctx, "m") },
			reached: state("m", machine.StateStopped),
		},
		{
			name: "Pause", arrange: inState(machine.StateRunning), hold: holdStore("save"),
			call:    func(ctx context.Context, svc machine.Service) error { return svc.Pause(ctx, "m") },
			reached: state("m", machine.StatePaused),
		},
		{
			name: "Resume", arrange: inState(machine.StatePaused), hold: holdStore("save"),
			call:    func(ctx context.Context, svc machine.Service) error { return svc.Resume(ctx, "m") },
			reached: state("m", machine.StateRunning),
		},
		{
			name: "Delete", arrange: inState(machine.StateStopped), hold: holdStore("remove"),
			call: func(ctx context.Context, svc machine.Service) error { return svc.Delete(ctx, "m") },
			reached: func(t *testing.T, _ *env, svc machine.Service) {
				_, err := svc.Inspect(context.Background(), "m")
				requireKind(t, err, machine.ErrNotFound)
			},
		},
		{
			name: "AttachDisk",
			arrange: func(t *testing.T, e *env) {
				e.disks.Provide("d")
				e.inState(t, "m", machine.StateCreated)
			},
			hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "d"})
			},
			reached: func(t *testing.T, _ *env, svc machine.Service) {
				m, err := svc.Inspect(context.Background(), "m")
				require.NoError(t, err)
				require.Len(t, m.Disks, 1)
			},
		},
		{
			name: "DetachDisk",
			arrange: func(t *testing.T, e *env) {
				e.disks.Provide("d")
				e.inState(t, "m", machine.StateCreated)
				require.NoError(t, e.svc.AttachDisk(context.Background(), "m", machine.DiskAttachment{Disk: "d"}))
			},
			hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error { return svc.DetachDisk(ctx, "m", "d") },
			reached: func(t *testing.T, _ *env, svc machine.Service) {
				m, err := svc.Inspect(context.Background(), "m")
				require.NoError(t, err)
				require.Empty(t, m.Disks)
			},
		},
		{
			name: "SetResources", arrange: inState(machine.StateCreated), hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.SetResources(ctx, "m", machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30})
			},
			reached: func(t *testing.T, _ *env, svc machine.Service) {
				m, err := svc.Inspect(context.Background(), "m")
				require.NoError(t, err)
				require.Equal(t, machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}, m.Resources)
			},
		},
		{
			name: "Snapshot", arrange: inState(machine.StateRunning),
			hold: func(e *env) (<-chan struct{}, func()) { return e.snaps.Hold("put") },
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.Snapshot(ctx, "m", "snap", "req-snap")
			},
			reached: func(t *testing.T, e *env, svc machine.Service) {
				require.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())
				state("m", machine.StateRunning)(t, e, svc)
			},
		},
		{
			name: "RestoreSnapshot",
			arrange: func(t *testing.T, e *env) {
				e.inState(t, "src", machine.StateRunning)
				require.NoError(t, e.svc.Snapshot(context.Background(), "src", "snap", "req-snap"))
			},
			hold: holdStore("save"),
			call: func(ctx context.Context, svc machine.Service) error {
				return svc.RestoreSnapshot(ctx, "m", "req-m", "snap", baseSpec().Policy)
			},
			reached: state("m", machine.StateRunning),
		},
	}
}

// cancelledMidCall runs the call, cancels its context while it is held in a
// step, lets the step go on, and returns the call's outcome.
func cancelledMidCall(t *testing.T, c acceptedCall, e *env, svc machine.Service) error {
	t.Helper()
	entered, release := c.hold(e)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.call(ctx, svc) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("the call never reached the step it is held at")
	}
	cancel()
	release()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end")
		return nil
	}
}

// An accepted call is not cut short by its caller going away: it reaches the
// state its success names. The control runs the same call on the caller's own
// context, and the cancellation fails it.
func TestAcceptedCall_OutlivesTheCallersCancellation(t *testing.T) {
	for _, c := range acceptedCalls() {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(allCaps...)
			c.arrange(t, e)
			svc := e.reopened(t)
			require.NoError(t, cancelledMidCall(t, c, e, svc))
			c.reached(t, e, svc)
		})
		t.Run(c.name+" on the caller's context fails", func(t *testing.T) {
			e := newEnv(allCaps...)
			c.arrange(t, e)
			svc := e.reopenedWith(t, machine.OnCallersContext())
			require.Error(t, cancelledMidCall(t, c, e, svc), "the control: the cancellation reaches the step")
		})
	}
}

// Snapshot's Resume after the capture, the case #55 names: a caller that
// leaves while the guest is paused does not leave the Machine Faulted.
func TestAcceptedCall_ASnapshotThatTheCallerLeavesResumesTheMachine(t *testing.T) {
	e := newEnv(allCaps...)
	e.inState(t, "m", machine.StateRunning)
	svc := e.reopened(t)
	c := acceptedCalls()[10]
	require.Equal(t, "Snapshot", c.name)

	require.NoError(t, cancelledMidCall(t, c, e, svc))
	m, err := svc.Inspect(context.Background(), "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateRunning, m.State)
	assert.Empty(t, m.FailureReason)
}

// A step that hangs is ended by the lifecycle's bound, whatever the caller's
// context says, and the call fails into a state its row names.
func TestAcceptedCall_AHangingStepIsEndedByTheBound(t *testing.T) {
	e := newEnv(allCaps...)
	e.inState(t, "m", machine.StateRunning)
	svc := e.reopenedWith(t, machine.WithCallTimeout(50*time.Millisecond))
	entered, release := e.store.Hold("save")

	done := make(chan error, 1)
	go func() { done <- svc.Pause(context.Background(), "m") }() // a context that never ends
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the call never reached the step")
	}
	time.Sleep(100 * time.Millisecond) // past the bound
	release()
	select {
	case err := <-done:
		requireKind(t, err, machine.ErrFailed)
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not end")
	}

	// Control: with the bound not reached, the same step goes on to success.
	svc = e.reopenedWith(t, machine.WithCallTimeout(time.Minute))
	entered, release = e.store.Hold("save")
	go func() { done <- svc.Pause(context.Background(), "m") }()
	<-entered
	time.Sleep(100 * time.Millisecond)
	release()
	require.NoError(t, <-done)
}

// Start's bound includes the boot timeout: a boot that finishes after the call
// timeout but within the boot timeout leaves the Machine Running and recorded.
// The control is a boot that never finishes, which ends Stopped with the
// boot-timeout reason and the network withdrawn.
func TestAcceptedCall_AStartsBoundIncludesTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	options := []machine.Option{machine.WithCallTimeout(100 * time.Millisecond), machine.WithBootTimeout(500 * time.Millisecond)}
	start := func(e *env, svc machine.Service, wait time.Duration, finish bool) error {
		entered, release := e.guest.Hold("boot")
		done := make(chan error, 1)
		go func() { done <- svc.Start(ctx, "m") }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			release()
			t.Fatal("the guest was never sent its boot description")
		}
		time.Sleep(wait)
		if finish {
			release()
		}
		select {
		case err := <-done:
			if !finish {
				release() // lets the abandoned boot call go
			}
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the start never ended")
			return nil
		}
	}

	e := newEnv(allCaps...)
	e.inState(t, "m", machine.StateCreated)
	svc := e.reopenedWith(t, options...)
	require.NoError(t, start(e, svc, 200*time.Millisecond, true), "the boot ended after the call timeout")
	m, err := svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateRunning, m.State)
	e.requireStored(t, "m", machine.StateRunning)

	e = newEnv(allCaps...)
	e.inState(t, "m", machine.StateCreated)
	svc = e.reopenedWith(t, options...)
	requireKind(t, start(e, svc, 0, false), machine.ErrFailed)
	m, err = svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateStopped, m.State)
	assert.Contains(t, m.FailureReason, "the guest did not finish booting within 500ms")
	_, tap := e.net.Applied("m")
	assert.False(t, tap, "the network is withdrawn")
}

// A call that the bound ends mid-step still reaches the failure state its row
// names: the steps that undo it run on a context of their own. The control runs
// them on the call's ended context, and the root disk is left behind.
func TestAcceptedCall_ACallTheBoundEndsIsCompensated(t *testing.T) {
	ctx := context.Background()
	run := func(t *testing.T, options ...machine.Option) (*env, machine.Service) {
		e := newEnv(allCaps...)
		svc := e.reopenedWith(t, append(options, machine.WithCallTimeout(50*time.Millisecond))...)
		entered, release := e.store.Hold("save")
		done := make(chan error, 1)
		go func() { done <- svc.Create(ctx, "m", "req-m", baseSpec()) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			release()
			t.Fatal("the call never reached the record step")
		}
		time.Sleep(100 * time.Millisecond) // past the bound
		release()
		select {
		case err := <-done:
			requireKind(t, err, machine.ErrFailed)
		case <-time.After(5 * time.Second):
			t.Fatal("the call did not end")
		}
		return e, svc
	}

	e, svc := run(t)
	_, err := svc.Inspect(ctx, "m")
	requireKind(t, err, machine.ErrNotFound)
	_, held := e.disks.Holds("m-root")
	assert.False(t, held, "no root disk is held")
	assert.False(t, e.net.Allocated("m"), "no address is held")

	e, _ = run(t, machine.CompensatingOnTheCallsContext())
	_, held = e.disks.Holds("m-root")
	assert.True(t, held, "the control: undone on the ended context, the disk is left behind")
}

// A repeat of a call, with the same identifier and request ID, while the first
// still runs waits for its outcome and returns it (runtime-api.md §Calling
// rules); the work is done once.
func TestAcceptedCall_ARepeatWhileTheFirstRunsWaitsForItsOutcome(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T, e *env)
		hold  func(e *env) (<-chan struct{}, func())
		call  func(svc machine.Service) error
		runs  func(e *env) int
		want  int
	}{
		{
			name: "Create", setup: func(*testing.T, *env) {}, hold: holdStore("save"),
			call: func(svc machine.Service) error { return svc.Create(context.Background(), "m", "req-m", baseSpec()) },
			runs: func(e *env) int { return e.disks.Calls("create-root") }, want: 1,
		},
		{
			name: "Start", setup: func(t *testing.T, e *env) { e.inState(t, "m", machine.StateCreated) }, hold: holdStore("save"),
			call: func(svc machine.Service) error { return svc.Start(context.Background(), "m") },
			runs: func(e *env) int { return e.guest.Calls("boot") }, want: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(allCaps...)
			tt.setup(t, e)
			svc := e.reopened(t)
			entered, release := tt.hold(e)
			first, second := make(chan error, 1), make(chan error, 1)
			go func() { first <- tt.call(svc) }()
			select {
			case <-entered:
			case <-time.After(5 * time.Second):
				release()
				t.Fatal("the first call never reached the step")
			}
			go func() { second <- tt.call(svc) }()

			select {
			case err := <-second:
				release()
				t.Fatalf("the repeat answered before the first call had an outcome: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			release()
			require.NoError(t, <-first)
			select {
			case err := <-second:
				require.NoError(t, err, "the repeat returns the first call's outcome")
			case <-time.After(5 * time.Second):
				t.Fatal("the repeat did not end")
			}
			assert.Equal(t, tt.want, tt.runs(e), "the work was done once")
		})
	}
}
