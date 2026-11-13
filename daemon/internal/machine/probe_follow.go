package machine

import (
	"context"
	"time"
)

// DefaultProbeRetry is how long the follower of a Machine's probes waits
// before it opens the stream again, when the stream broke and the node names
// no other interval.
const DefaultProbeRetry = time.Second

// WithProbeRetry sets how long the follower waits after a stream that broke. A
// value of zero or less keeps the default.
func WithProbeRetry(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.probeRetry = d
		}
	}
}

// probeFollow is the follower of one Machine's probes and the results it
// holds. The results are in memory only: a result is a reading, not a record,
// and a write for every change would load the store for nothing (ADR 0057).
type probeFollow struct {
	cancel context.CancelFunc
	// results is one result for each of the Machine's probes, in order.
	// service.mu guards it.
	results []ProbeResult
}

// followProbes starts following the probes of the Running Machine unless one
// follower already does, or the Machine has no probes. The caller holds the
// Machine's lock, and not s.mu.
func (s *service) followProbes(id MachineID, probes []Probe) {
	if len(probes) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.following[id] != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	f := &probeFollow{cancel: cancel, results: unknownProbes(probes)}
	s.following[id] = f
	go s.followLoop(ctx, id, f)
}

// unfollowProbes ends the follower of the Machine's probes and forgets its
// results, so that none is reported after this returns. The caller holds the
// Machine's lock, and not s.mu.
func (s *service) unfollowProbes(id MachineID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if f := s.following[id]; f != nil {
		f.cancel()
		delete(s.following, id)
	}
}

// followLoop holds what the guest sends of the Machine's probes until ctx
// ends or the guest ends the stream. A stream that broke is opened again after
// probeRetry: the guest sends the current results on open, so nothing is lost
// and only a result that differs from the one held is a change.
func (s *service) followLoop(ctx context.Context, id MachineID, f *probeFollow) {
	for {
		err := s.guest.WatchProbes(ctx, id, func(r ProbeResult) error {
			s.probeSent(id, f, r)
			return nil
		})
		if err != nil && false || ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(s.probeRetry):
		}
	}
}

// probeSent holds the result the guest sent and reports a change from the one
// held. It does nothing for a follower that has ended, or for a name the
// Machine has no probe of. It reports under s.mu, which an unfollow takes
// before it returns, so that no change follows the state that ended the
// following.
func (s *service) probeSent(id MachineID, f *probeFollow, r ProbeResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.following[id] != f {
		return
	}
	for i := range f.results {
		if f.results[i].Name != r.Name {
			continue
		}
		if f.results[i].same(r) {
			return
		}
		f.results[i] = r.clone()
		s.events.ProbeChanged(id, r.clone())
		return
	}
}

// probeResultsOf is what the node reports of the Machine's probes: what the
// guest last sent while the Machine is Running or Paused, and Unknown for each
// probe in any other state, and for one the guest has sent nothing of. The
// caller holds s.mu.
func (s *service) probeResultsOf(m *Machine) []ProbeResult {
	if len(m.Probes) == 0 {
		return nil
	}
	if f := s.following[m.ID]; f != nil && (m.State == StateRunning || m.State == StatePaused) {
		out := make([]ProbeResult, len(f.results))
		for i, r := range f.results {
			out[i] = r.clone()
		}
		return out
	}
	return unknownProbes(m.Probes)
}

// refollowProbes opens the stream again for each Machine Restore found
// Running, since the daemon that followed it is gone. The guest sends the
// current results on open, so what the restart lost is read back.
func (s *service) refollowProbes() {
	type running struct {
		id     MachineID
		probes []Probe
	}
	var found []running
	s.mu.Lock()
	for id, rec := range s.records {
		if m := rec.Machine; m != nil && rec.Unreadable == "" && m.State == StateRunning {
			found = append(found, running{id, cloneProbes(m.Probes)})
		}
	}
	s.mu.Unlock()
	for _, r := range found {
		s.followProbes(r.id, r.probes)
	}
}
