package events

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Service is the event log. The lifecycle and the deadline write to it through
// their own ports; a driving adapter reads it through Watch.
type Service interface {
	machine.Events
	fencing.Events
	// Watch starts a watcher after the sequence: it delivers the held events
	// that follow it and then each new one, as one ordered sequence. Zero
	// starts at the oldest event held. A sequence equal to the latest one is
	// caught up and waits for the next event.
	Watch(after uint64) Watcher
}

// Watcher reads the log from where it started. One goroutine uses it, and
// nothing needs releasing.
type Watcher interface {
	// Next returns the event after the last one it returned, waiting for it
	// when there is none yet, until ctx ends. It returns ErrResync, and from
	// then on only ErrResync, when that event is no longer held: a watcher
	// that falls behind the retention is never given a stream with a hole.
	Next(ctx context.Context) (Event, error)
}

type service struct {
	keep int

	// mu guards the fields below.
	mu sync.Mutex
	// held is the latest events, oldest first, at most keep of them.
	held []Event
	// last is the sequence of the latest event, zero before the first.
	last uint64
	// changed is closed, and replaced, by every event, to wake the watchers
	// that wait.
	changed chan struct{}
}

// NewService returns a log that holds the latest keep events.
func NewService(keep int) Service {
	return &service{keep: max(keep, 0), changed: make(chan struct{})}
}

// oldest is the sequence of the oldest event held, or the next one when none
// is. The caller holds mu.
func (s *service) oldest() uint64 { return s.last - uint64(len(s.held)) + 1 }

func (s *service) emit(id machine.MachineID, body Body) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.last++
	s.held = append(s.held, Event{Sequence: s.last, Machine: id, Body: body})
	if len(s.held) > s.keep {
		s.held = s.held[len(s.held)-s.keep:]
	}
	close(s.changed)
	s.changed = make(chan struct{})
}

// StateChanged implements machine.Events.
func (s *service) StateChanged(id machine.MachineID, state machine.State) {
	s.emit(id, StateChanged{State: state})
}

// Reported implements machine.Events.
func (s *service) Reported(event machine.DriverEvent) {
	s.emit("", Driver{Event: event})
}

// Deadline implements fencing.Events.
func (s *service) Deadline(event fencing.Event) {
	s.emit("", Deadline{Event: event})
}

func (s *service) Watch(after uint64) Watcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if after == 0 {
		after = s.oldest() - 1
	}
	return &watcher{s: s, after: after}
}

type watcher struct {
	s *service
	// after is the sequence of the last event returned, or the one to resume
	// after.
	after uint64
	// resync is set once ErrResync was returned, and stays: a log that grows
	// past a sequence it never held does not close the hole.
	resync bool
}

func (w *watcher) Next(ctx context.Context) (Event, error) {
	s := w.s
	for {
		s.mu.Lock()
		oldest := s.oldest()
		switch {
		case w.resync, w.after > s.last, w.after < oldest-1:
			// Ahead of this log, which a restart empties, or behind what it
			// still holds: either way the client's picture has a hole.
			s.mu.Unlock()
			w.resync = true
			return Event{}, ErrResync
		case w.after < s.last:
			e := s.held[w.after+1-oldest]
			s.mu.Unlock()
			w.after++
			return e, nil
		}
		changed := s.changed
		s.mu.Unlock()

		select {
		case <-changed:
		case <-ctx.Done():
			return Event{}, ctx.Err()
		}
	}
}
