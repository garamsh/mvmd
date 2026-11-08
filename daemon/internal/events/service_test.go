package events_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// next reads the watcher's next event, which the test knows is held or coming.
func next(t *testing.T, w events.Watcher) events.Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e, err := w.Next(ctx)
	require.NoError(t, err)
	return e
}

// requireWaiting requires the watcher to have nothing to return yet: its Next
// blocks until the context ends, and does not report a resync.
func requireWaiting(t *testing.T, w events.Watcher) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := w.Next(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func requireResync(t *testing.T, w events.Watcher) {
	t.Helper()
	_, err := w.Next(context.Background())
	require.ErrorIs(t, err, events.ErrResync)
}

func emitDriver(s events.Service, n int) {
	for i := 1; i <= n; i++ {
		s.Reported(machine.DriverEvent{Name: fmt.Sprint(i)})
	}
}

func TestWatch_ReplaysHeldEventsThenDeliversNewOnesInOneSequence(t *testing.T) {
	s := events.NewService(10)
	s.StateChanged("a", machine.StateCreated)
	s.Deadline(fencing.EventArmed)
	w := s.Watch(0)

	first := next(t, w)
	assert.Equal(t, uint64(1), first.Sequence)
	assert.Equal(t, machine.MachineID("a"), first.Machine)
	assert.Equal(t, events.StateChanged{State: machine.StateCreated}, first.Body)
	second := next(t, w)
	assert.Equal(t, uint64(2), second.Sequence)
	assert.Empty(t, second.Machine, "a deadline event belongs to the node")
	assert.Equal(t, events.Deadline{Event: fencing.EventArmed}, second.Body)

	// Control: after the replay is drained the stream is live, and an event
	// emitted now reaches the watcher that already drained it.
	requireWaiting(t, w)
	s.Reported(machine.DriverEvent{Name: "exited", Detail: "signal 9"})
	third := next(t, w)
	assert.Equal(t, uint64(3), third.Sequence)
	assert.Equal(t, events.Driver{Event: machine.DriverEvent{Name: "exited", Detail: "signal 9"}}, third.Body)
}

func TestWatch_AWaitingWatcherIsWokenByTheNextEvent(t *testing.T) {
	s := events.NewService(10)
	w := s.Watch(0)
	got := make(chan events.Event, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		e, _ := w.Next(ctx)
		got <- e
	}()
	s.StateChanged("a", machine.StateRunning)
	assert.Equal(t, uint64(1), (<-got).Sequence)
}

func TestWatch_ResumesAfterASequence(t *testing.T) {
	s := events.NewService(10)
	emitDriver(s, 5)

	assert.Equal(t, uint64(4), next(t, s.Watch(3)).Sequence)
	// Control: zero starts at the oldest held, not after anything.
	assert.Equal(t, uint64(1), next(t, s.Watch(0)).Sequence)
}

func TestWatch_AWatcherAtTheHeadWaitsInsteadOfResyncing(t *testing.T) {
	s := events.NewService(10)
	emitDriver(s, 3)

	w := s.Watch(3)
	requireWaiting(t, w)
	s.Reported(machine.DriverEvent{Name: "4"})
	assert.Equal(t, uint64(4), next(t, w).Sequence)

	// Control: a log with no event yet is the same case at sequence zero.
	empty := events.NewService(10)
	requireWaiting(t, empty.Watch(0))
}

func TestWatch_ResyncsOnlyWhenTheEventsAfterTheSequenceWereDropped(t *testing.T) {
	s := events.NewService(3)
	emitDriver(s, 10) // holds 8, 9 and 10

	requireResync(t, s.Watch(6)) // 7 was dropped
	// Controls: the boundary, one event further back, is still whole.
	assert.Equal(t, uint64(8), next(t, s.Watch(7)).Sequence)
	assert.Equal(t, uint64(8), next(t, s.Watch(0)).Sequence)
}

func TestWatch_ResyncsAWatcherAheadOfTheLog(t *testing.T) {
	// A restart empties the log, so a client's sequence can be ahead of it.
	s := events.NewService(100)
	emitDriver(s, 2)

	w := s.Watch(50)
	requireResync(t, w)
	// The log growing past the client's number does not close its hole.
	emitDriver(s, 60)
	requireResync(t, w)
}

func TestWatch_AWatcherThatFallsBehindEndsWithResyncNeverAHole(t *testing.T) {
	s := events.NewService(3)
	emitDriver(s, 2)
	w := s.Watch(0)
	assert.Equal(t, uint64(1), next(t, w).Sequence)

	// Control: still within the retention, the watcher goes on in order.
	emitDriver(s, 1)
	assert.Equal(t, uint64(2), next(t, w).Sequence)
	assert.Equal(t, uint64(3), next(t, w).Sequence)

	emitDriver(s, 4) // 4..7; holds 5, 6 and 7, so 4 is gone
	requireResync(t, w)
	requireResync(t, w)
}

func TestWatch_ManyConcurrentWritersAreOneConsecutiveSequence(t *testing.T) {
	const writers, each = 8, 50
	s := events.NewService(writers * each)
	w := s.Watch(0)

	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := machine.MachineID(fmt.Sprint("m", i))
			for range each {
				s.StateChanged(id, machine.StateRunning)
			}
		}()
	}
	for want := uint64(1); want <= writers*each; want++ {
		assert.Equal(t, want, next(t, w).Sequence)
	}
	wg.Wait()
}
