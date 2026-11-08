package machine

// LockCount returns how many per-Machine locks the service holds.
func LockCount(s Service) int {
	svc := s.(*service)
	svc.mu.Lock()
	defer svc.mu.Unlock()
	return len(svc.locks)
}
