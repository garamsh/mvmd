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

// CompensatingOnTheCallsContext makes a service run the steps that undo a
// failed call on the call's own context; it is the control of the tests that
// end a call by its bound.
func CompensatingOnTheCallsContext() Option {
	return func(s *service) {
		s.compensate = func(ctx context.Context, _ time.Duration) (context.Context, context.CancelFunc) {
			return ctx, func() {}
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
