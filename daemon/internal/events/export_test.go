package events

// LastSequence returns the current sequence counter for tests.
func LastSequence(s Store) uint64 {
	st := s.(*store)
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.sequence
}

// EventCount returns the number of events retained in the store for tests.
func EventCount(s Store) int {
	st := s.(*store)
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.events)
}
