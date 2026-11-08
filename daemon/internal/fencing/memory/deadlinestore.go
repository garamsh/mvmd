package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// DeadlineStore is an in-memory fencing.DeadlineStore. Its steps are "load"
// and "save".
type DeadlineStore struct {
	machinememory.Failures

	mu         sync.Mutex
	rec        fencing.Record
	have       bool
	supervisor *Supervisors
	violations []string
}

// NewDeadlineStore returns a store with no record.
func NewDeadlineStore() *DeadlineStore { return &DeadlineStore{} }

// Load implements fencing.DeadlineStore.
func (s *DeadlineStore) Load(context.Context) (fencing.Record, bool, error) {
	if err := s.Enter("load"); err != nil {
		return fencing.Record{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.have, nil
}

// Save implements fencing.DeadlineStore. If a live supervisor then holds an
// expiry later than the saved one, the moment is recorded as a violation.
func (s *DeadlineStore) Save(_ context.Context, record fencing.Record) error {
	if err := s.Enter("save"); err != nil {
		return err
	}
	s.mu.Lock()
	s.rec, s.have = record, true
	sup := s.supervisor
	s.mu.Unlock()
	if sup != nil {
		for id, e := range sup.held() {
			if e > record.Expiry {
				s.violate(fmt.Sprintf("after saving %v, supervisor of %q holds the later %v", record.Expiry, id, e))
			}
		}
	}
	return nil
}

// Recorded returns the record, if one was saved.
func (s *DeadlineStore) Recorded() (fencing.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.have
}

// Violations lists every moment a supervisor held an expiry later than the
// recorded one. A correct renewal leaves it empty.
func (s *DeadlineStore) Violations() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.violations...)
}

func (s *DeadlineStore) violate(what string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.violations = append(s.violations, what)
}
