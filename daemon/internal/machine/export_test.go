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

// SnapshotLockCount returns how many snapshot locks the service holds.
func SnapshotLockCount(s Service) int {
	return s.(*service).snapLocks.Held()
}

// DiskLockUsers returns how many Create disk calls hold or wait for the disk's
// lock.
func DiskLockUsers(s Service, disk DiskID) int {
	return s.(*service).diskLocks.Users(disk)
}

// ObserveEnds runs one look for the VMMs that have ended, as Watch does on each
// tick.
func ObserveEnds(ctx context.Context, s Service) {
	s.(*service).observeEnds(ctx)
}

// RestartDue runs one pass of the restarts Watch begins on each tick, and
// returns when the starts it began have ended.
func RestartDue(ctx context.Context, s Service) {
	s.(*service).restartDue(ctx, true)
}

// SettleEnd settles one Machine as a look does once its listing named it, so a
// test can give it a listing that is stale.
func SettleEnd(ctx context.Context, s Service, id MachineID) {
	s.(*service).settleEnd(ctx, id)
}
