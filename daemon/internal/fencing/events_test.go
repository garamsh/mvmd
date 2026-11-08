package fencing_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	armed   = fencing.EventArmed
	renewed = fencing.EventRenewed
	expired = fencing.EventExpired
)

func TestEvents_RenewalsAreReportedAsArmedThenRenewed(t *testing.T) {
	e := newEnv(t)
	assert.Empty(t, e.events.Seen(), "nothing before the first renewal")

	e.renew(t, time.Minute, 1)
	assert.Equal(t, []fencing.Event{armed}, e.events.Seen())

	e.renew(t, time.Minute, 2)
	assert.Equal(t, []fencing.Event{armed, renewed}, e.events.Seen())

	// Control: a number that is not greater changes nothing, and says nothing.
	e.renew(t, time.Hour, 2)
	assert.Equal(t, []fencing.Event{armed, renewed}, e.events.Seen())
}

func TestEvents_ARenewalAfterAnExpiryArmsAgain(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.clock.Advance(2 * time.Minute)
	e.renew(t, time.Minute, 2)
	assert.Equal(t, []fencing.Event{armed, armed}, e.events.Seen())
}

func TestEvents_ARenewalThatCannotBeRecordedIsNotReported(t *testing.T) {
	e := newEnv(t)
	e.store.FailAt("save", errBoom)
	_, err := e.svc.Renew(context.Background(), time.Minute, 1)
	requireKind(t, err, machine.ErrFailed)
	assert.Empty(t, e.events.Seen())
}

func TestEvents_TheExpiryIsReportedOnceEverySupervisorIsGone(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a")
	stop := e.run(t)
	defer stop()

	e.clock.Advance(30 * time.Second)
	assert.Equal(t, []fencing.Event{armed}, e.events.Seen(), "control: before the expiry")

	e.clock.Advance(time.Minute)
	e.eventually(t, "the expiry is reported", func() bool { return len(e.events.Seen()) == 2 })
	assert.Equal(t, []fencing.Event{armed, expired}, e.events.Seen())
	live, err := e.sup.Live(context.Background())
	require.NoError(t, err)
	assert.Empty(t, live, "reported with the supervisors gone")
}

func TestEvents_AReconciliationReportsWhatItFound(t *testing.T) {
	t.Run("a recorded expiry still ahead is armed", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		require.NoError(t, e.restart(t))
		assert.Equal(t, []fencing.Event{armed, armed}, e.events.Seen())
	})
	t.Run("a passed expiry is expired", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.clock.Advance(2 * time.Minute)
		require.NoError(t, e.restart(t))
		assert.Equal(t, []fencing.Event{armed, expired}, e.events.Seen())
	})
	t.Run("a reboot leaves the deadline unarmed and says nothing", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.clock.Reboot("boot-2")
		require.NoError(t, e.restart(t))
		assert.Equal(t, []fencing.Event{armed}, e.events.Seen())
	})
}
