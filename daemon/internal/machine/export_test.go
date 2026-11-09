package machine

import (
	"context"
	"time"
)

// OnCallersContext makes a service run each call on its caller's context, as
// the lifecycle did before accepted calls were detached; it is the control of
// the tests that cancel a caller mid-call.
func OnCallersContext() Option {
	return func(s *service) {
		s.callContext = func(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
			return context.WithTimeout(ctx, bound)
		}
	}
}

// LockCount returns how many per-Machine locks the service holds.
func LockCount(s Service) int {
	svc := s.(*service)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return len(svc.locks)
}
