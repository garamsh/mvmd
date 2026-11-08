// Package events provides the in-process event store that powers the
// WatchEvents RPC. It assigns a monotonically growing sequence number to
// every event, retains events for a constructor-specified count, and
// notifies a watcher when a resume sequence is no longer held.
package events

import (
	"context"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// Event is a single data point emitted on every state transition, deadline
// change, and driver notification.
type Event struct {
	Sequence  uint64
	MachineID string
	Body      any // *StateChanged, *DeadlineEvent, *DriverEvent
}

// Emitter is the port the lifecycle and the deadline call to emit events.
// The inside passes no technology type across it.
type Emitter interface {
	EmitStateChanged(id string, state v1alpha1.MachineState)
	EmitDeadline(kind v1alpha1.DeadlineEvent_Kind)
	EmitDriver(name, detail string)
}

// Watcher is what a store hands a caller that subscribes. The caller reads
// events until the context is cancelled, the stream is closed, or the
// watcher signals a resync is required.
type Watcher interface {
	// Done is closed when the watcher is no longer valid (the context ended
	// or the store closed it because a resync was required).
	Done() <-chan struct{}
	// Events returns the read channel. Each entry carries the sequence,
	// the machine identifier, and the event body.
	Events() <-chan Event
	// Close releases the watcher's slot in the store.
	Close()
}

// Store is the event store: it accepts events from the inside, holds them
// in sequence order, and serves subscribers that may resume from a
// sequence number.
type Store interface {
	Emitter
	// Subscribe starts a watcher that begins at the oldest event or, if
	// after is greater than zero, at the first event with a sequence
	// greater than after. If the store no longer holds that far back, it
	// signals the watcher to close and the caller must resync.
	Subscribe(after uint64) Watcher
}

type eventKind int

const (
	eventStateChanged eventKind = iota
	eventDeadline
	eventDriver
)

type entry struct {
	sequence  uint64
	machineID string
	kind      eventKind
	body      any
}

type store struct {
	mu        sync.Mutex
	events    []entry
	sequence  uint64
	retention int
	watchers  []*watcher
}

// New returns a store that retains the last keep events.
func New(keep int) Store {
	return &store{retention: keep}
}

func (s *store) EmitStateChanged(id string, state v1alpha1.MachineState) {
	s.emit(eventStateChanged, id, &v1alpha1.StateChanged{State: state})
}

func (s *store) EmitDeadline(kind v1alpha1.DeadlineEvent_Kind) {
	s.emit(eventDeadline, "", &v1alpha1.DeadlineEvent{Kind: kind})
}

func (s *store) EmitDriver(name, detail string) {
	s.emit(eventDriver, "", &v1alpha1.DriverEvent{Name: name, Detail: detail})
}

func (s *store) emit(kind eventKind, machineID string, body any) {
	s.mu.Lock()
	s.sequence++
	s.events = append(s.events, entry{
		sequence:  s.sequence,
		machineID: machineID,
		kind:      kind,
		body:      body,
	})
	if s.retention > 0 && len(s.events) > s.retention {
		s.events = s.events[len(s.events)-s.retention:]
	}
	ws := s.watchers
	s.mu.Unlock()

	for _, w := range ws {
		select {
		case w.ch <- Event{
			Sequence:  s.sequence,
			MachineID: machineID,
			Body:      body,
		}:
		default:
			// Drop if the watcher's buffer is full; the caller
			// may lag behind the producer.
		}
	}
}

func (s *store) Subscribe(after uint64) Watcher {
	w := &watcher{store: s, done: make(chan struct{}), ch: make(chan Event, 128)}
	s.mu.Lock()
	s.watchers = append(s.watchers, w)
	if after > 0 {
		start := slices.IndexFunc(s.events, func(e entry) bool {
			return e.sequence > after
		})
		if start < 0 || start >= len(s.events) {
			// No events after the requested sequence; the client
			// must resync.
			w.once.Do(func() { close(w.done) })
			close(w.ch)
			w.mu.Lock()
			w.resync = true
			w.mu.Unlock()
			s.mu.Unlock()
			return w
		}
		for i := start; i < len(s.events); i++ {
			e := s.events[i]
			w.ch <- Event{
				Sequence:  e.sequence,
				MachineID: e.machineID,
				Body:      e.body,
			}
		}
		close(w.ch)
		s.mu.Unlock()
		return w
	}
	if len(s.events) == 0 {
		// No events to replay; signal the watcher immediately.
		w.once.Do(func() { close(w.done) })
		close(w.ch)
		s.mu.Unlock()
		return w
	}
	// Replay all events then close — snapshot semantics.
	for _, e := range s.events {
		w.ch <- Event{
			Sequence:  e.sequence,
			MachineID: e.machineID,
			Body:      e.body,
		}
	}
	close(w.ch)
	s.mu.Unlock()
	return w
}

type watcher struct {
	store  *store
	done   chan struct{}
	ch     chan Event
	resync bool
	mu     sync.Mutex
	once   sync.Once
}

func (w *watcher) Done() <-chan struct{} { return w.done }

func (w *watcher) Events() <-chan Event {
	if w.resync {
		return nil
	}
	return w.ch
}

func (w *watcher) Close() {
	w.mu.Lock()
	if w.resync {
		w.mu.Unlock()
		return
	}
	w.mu.Unlock()
	w.store.mu.Lock()
	for i, ww := range w.store.watchers {
		if ww == w {
			w.store.watchers = slices.Clone(w.store.watchers)
			w.store.watchers[i] = w.store.watchers[len(w.store.watchers)-1]
			w.store.watchers[len(w.store.watchers)-1] = nil
			w.store.watchers = w.store.watchers[:len(w.store.watchers)-1]
			break
		}
	}
	w.store.mu.Unlock()
	w.once.Do(func() { close(w.done) })
}

// Drain consumes every event in the store up to the given limit,
// skipping those with sequence <= after, and returns them as a slice.
func Drain(ctx context.Context, store Store, after uint64, limit int) ([]Event, error) {
	w := store.Subscribe(after)
	defer w.Close()

	var events []Event
	if w.Events() == nil {
		return nil, nil
	}
	for e := range w.Events() {
		events = append(events, e)
		if len(events) >= limit {
			break
		}
	}
	return events, nil
}
