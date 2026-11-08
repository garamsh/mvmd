package fencing_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Renew deadline: "The first renewal arms the deadline."
func TestRenew_FirstRenewalArms(t *testing.T) {
	e := newEnv(t)
	assert.Equal(t, fencing.PhaseUnarmed, e.status(t).Phase)

	st := e.renew(t, 10*time.Second, 7)
	assert.Equal(t, fencing.Status{Phase: fencing.PhaseArmed, Length: 10 * time.Second, Remaining: 10 * time.Second, Renewal: 7}, st)
	assert.Equal(t, st, e.status(t))
	rec, ok := e.store.Recorded()
	require.True(t, ok)
	assert.Equal(t, fencing.Record{Expiry: e.clock.Now().Add(10 * time.Second), BootID: "boot-1", Length: 10 * time.Second, Renewal: 7}, rec)
}

// Renew deadline: "If the number is greater than the last applied, the
// deadline becomes the length from the moment mvmd receives the call."
func TestRenew_GreaterNumberSetsTheLengthFromReceipt(t *testing.T) {
	e := newEnv(t)
	e.renew(t, 10*time.Second, 1)
	e.clock.Advance(4 * time.Second)

	st := e.renew(t, 10*time.Second, 2)
	assert.Equal(t, 10*time.Second, st.Remaining, "counted from the receipt of the second call")
	assert.Equal(t, uint64(2), st.Renewal)
}

// Renew deadline: "otherwise nothing changes and the current expiry is
// returned"; "A duplicate or a delayed older renewal ... cannot extend".
func TestRenew_NotGreaterChangesNothing(t *testing.T) {
	for _, number := range []uint64{5, 4, 1} {
		e := newEnv(t)
		first := e.renew(t, 10*time.Second, 5)
		e.running(t, "a")
		saves := e.store.Calls("save")
		e.clock.Advance(3 * time.Second)

		st, err := e.svc.Renew(context.Background(), time.Hour, number)
		require.NoError(t, err)
		assert.Equal(t, 7*time.Second, st.Remaining, "the current expiry, not an hour from now")
		assert.Equal(t, first.Renewal, st.Renewal)
		assert.Equal(t, first.Length, st.Length)
		assert.Equal(t, saves, e.store.Calls("save"), "nothing recorded")
		e.requireSupervisorsAt(t)

		// Control: the next number is applied.
		assert.Equal(t, time.Hour, e.renew(t, time.Hour, 6).Remaining)
	}
}

// Renew deadline: "mvmd imposes no minimum or maximum".
func TestRenew_AnyLength(t *testing.T) {
	e := newEnv(t)
	assert.Equal(t, time.Nanosecond, e.renew(t, time.Nanosecond, 1).Remaining)
	assert.Equal(t, 1000*time.Hour, e.renew(t, 1000*time.Hour, 2).Remaining, "longer is accepted")
	assert.Equal(t, time.Second, e.renew(t, time.Second, 3).Remaining, "shorter is accepted")

	for _, length := range []time.Duration{0, -time.Second} {
		_, err := e.svc.Renew(context.Background(), length, 4)
		requireKind(t, err, machine.ErrInvalid)
	}
	assert.Equal(t, uint64(3), e.status(t).Renewal, "an invalid renewal changes nothing")
}

// Renew deadline: "Renewal is acknowledged only after it is durably recorded."
func TestRenew_NotAcknowledgedUnlessRecorded(t *testing.T) {
	e := newEnv(t)
	e.store.FailAt("save", errBoom)

	_, err := e.svc.Renew(context.Background(), time.Minute, 1)
	requireKind(t, err, machine.ErrFailed)
	assert.Equal(t, fencing.PhaseUnarmed, e.status(t).Phase, "not armed by a renewal that was not recorded")

	e.store.Heal("save")
	e.renew(t, time.Minute, 1) // Control: the same renewal now applies.

	e.store.FailAt("save", errBoom)
	_, err = e.svc.Renew(context.Background(), time.Hour, 2)
	requireKind(t, err, machine.ErrFailed)
	assert.Equal(t, time.Minute, e.status(t).Length, "the held deadline is the recorded one")
	assert.Equal(t, uint64(1), e.status(t).Renewal)
}

func TestRenew_RefusedBeforeReconcile(t *testing.T) {
	store := memory.NewDeadlineStore()
	svc := fencing.NewService(memory.NewClock("boot-1"), store, memory.NewSupervisors(store), memory.NewEvents())
	_, err := svc.Renew(context.Background(), time.Minute, 1)
	requireKind(t, err, machine.ErrPreconditionFailed)
}

// Inspect deadline: "the length, the time remaining, the last applied renewal
// number, and whether it is armed, expired, or stopped everything".
func TestInspect_ReportsLengthRemainingNumberAndPhase(t *testing.T) {
	e := newEnv(t)
	e.renew(t, 10*time.Second, 3)
	e.clock.Advance(4 * time.Second)
	assert.Equal(t, fencing.Status{Phase: fencing.PhaseArmed, Length: 10 * time.Second, Remaining: 6 * time.Second, Renewal: 3}, e.status(t))

	e.clock.Advance(6 * time.Second)
	assert.Equal(t, fencing.Status{Phase: fencing.PhaseExpired, Length: 10 * time.Second, Renewal: 3}, e.status(t))

	stop := e.run(t)
	defer stop()
	e.eventually(t, "the phase becomes stopped everything", func() bool {
		return e.status(t).Phase == fencing.PhaseStopped
	})
	assert.Equal(t, uint64(3), e.status(t).Renewal)
}

// ADR 0003 and runtime-api.md: a renewal reaches every live supervisor before
// it is acknowledged, in either direction.
func TestRenew_EverySupervisorHoldsTheAcknowledgedExpiry(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a", "b", "c")
	e.requireSupervisorsAt(t)

	e.renew(t, time.Hour, 2) // an extension
	e.requireSupervisorsAt(t)
	e.renew(t, time.Second, 3) // a shortening
	e.requireSupervisorsAt(t)
	for _, id := range []machine.MachineID{"a", "b", "c"} {
		expiry, ok := e.sup.Expiry(id)
		require.True(t, ok)
		assert.Equal(t, e.clock.Now().Add(time.Second), expiry)
	}
}

// ADR 0003: an extension is recorded first and pushed second.
func TestRenew_Extension_StateAfterEachFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("the record fails: no supervisor changes and nothing is acknowledged", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a", "b")
		before, _ := e.sup.Expiry("a")
		e.store.FailAt("save", errBoom)

		_, err := e.svc.Renew(ctx, time.Hour, 2)
		requireKind(t, err, machine.ErrFailed)
		after, _ := e.sup.Expiry("a")
		assert.Equal(t, before, after)
		assert.Equal(t, uint64(1), e.status(t).Renewal)
		e.requireSupervisorsAt(t)
	})

	t.Run("a supervisor does not take it: not acknowledged, none holds more than the record, and the repeat completes it", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a", "b")
		e.sup.FailAt("set-expiry:b", errBoom)

		_, err := e.svc.Renew(ctx, time.Hour, 2)
		requireKind(t, err, machine.ErrFailed)
		assert.Empty(t, e.store.Violations(), "a supervisor that missed an extension only stops early")
		rec, _ := e.store.Recorded()
		held, _ := e.sup.Expiry("b")
		assert.Less(t, held, rec.Expiry)

		// The client repeats the renewal; the number is applied already, and
		// the answer is true only once every supervisor holds the expiry.
		_, err = e.svc.Renew(ctx, time.Hour, 2)
		requireKind(t, err, machine.ErrFailed)
		e.sup.Heal("set-expiry:b")
		st, err := e.svc.Renew(ctx, time.Hour, 2)
		require.NoError(t, err)
		assert.Equal(t, uint64(2), st.Renewal)
		e.requireSupervisorsAt(t)
	})
}

// ADR 0003: a shortening is pushed first and recorded second; a supervisor
// that fails to take it is killed before it is recorded.
func TestRenew_Shortening_StateAfterEachFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a supervisor that will not take it is killed and its Machine stopped", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.running(t, "a", "b")
		e.sup.FailAt("set-expiry:b", errBoom)

		st, err := e.svc.Renew(ctx, time.Minute, 2)
		require.NoError(t, err)
		assert.Equal(t, time.Minute, st.Remaining)
		_, alive := e.sup.Expiry("b")
		assert.False(t, alive, "the supervisor that kept the longer expiry is dead")
		assert.Equal(t, machine.StateStopped, e.state(t, "b"))
		assert.Equal(t, machine.StateRunning, e.state(t, "a"))
		e.requireSupervisorsAt(t)
	})

	t.Run("one that will neither take it nor die fails the renewal and records nothing", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.running(t, "a", "b")
		e.sup.FailAt("set-expiry:b", errBoom)
		e.sup.FailAt("terminate:b", errBoom)
		saves := e.store.Calls("save")

		_, err := e.svc.Renew(ctx, time.Minute, 2)
		requireKind(t, err, machine.ErrFailed)
		assert.Equal(t, saves, e.store.Calls("save"), "the shorter expiry was not recorded")
		assert.Equal(t, uint64(1), e.status(t).Renewal)
		assert.Empty(t, e.store.Violations())
	})

	t.Run("the record fails after the push: supervisors hold the shorter expiry, the record the longer", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.running(t, "a", "b")
		e.store.FailAt("save", errBoom)

		_, err := e.svc.Renew(ctx, time.Minute, 2)
		requireKind(t, err, machine.ErrFailed)
		rec, _ := e.store.Recorded()
		for _, id := range []machine.MachineID{"a", "b"} {
			held, _ := e.sup.Expiry(id)
			assert.Less(t, held, rec.Expiry, "an early stop is the only cost")
		}
		assert.Empty(t, e.store.Violations())

		// Control: the repeat with a greater number, recorded, brings all to it.
		e.store.Heal("save")
		e.renew(t, time.Minute, 3)
		e.requireSupervisorsAt(t)
	})

	t.Run("the supervisors cannot be listed", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.sup.FailAt("live", errBoom)
		_, err := e.svc.Renew(ctx, time.Minute, 2)
		requireKind(t, err, machine.ErrFailed)
		assert.Equal(t, uint64(1), e.status(t).Renewal)
	})
}
