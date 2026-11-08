package memory

import (
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

// Events is an in-memory fencing.Events that records what it was told.
type Events struct {
	mu   sync.Mutex
	seen []fencing.Event
}

// NewEvents returns an Events that has recorded nothing.
func NewEvents() *Events { return &Events{} }

// Deadline implements fencing.Events.
func (e *Events) Deadline(event fencing.Event) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seen = append(e.seen, event)
}

// Seen returns every event recorded, in order.
func (e *Events) Seen() []fencing.Event {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]fencing.Event(nil), e.seen...)
}
