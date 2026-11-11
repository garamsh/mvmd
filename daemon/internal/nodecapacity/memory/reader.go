// Package memory is the in-memory nodecapacity.Reader.
package memory

import (
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
)

// Reader is an in-memory nodecapacity.Reader that reports what it was set to.
type Reader struct {
	mu       sync.Mutex
	capacity nodecapacity.Capacity
	err      error
}

// NewReader returns a Reader reporting the capacity.
func NewReader(c nodecapacity.Capacity) *Reader { return &Reader{capacity: c} }

// Read implements nodecapacity.Reader.
func (r *Reader) Read() (nodecapacity.Capacity, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.capacity, r.err
}

// Set changes the capacity the Reader reports.
func (r *Reader) Set(c nodecapacity.Capacity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.capacity = c
}

// Fail makes Read return err; nil restores it.
func (r *Reader) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.err = err
}
