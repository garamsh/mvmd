package imagestore

// SetFault makes the Store ask fault before each step of Publish, and fail the
// step with what it returns.
func (s *Store) SetFault(fault func(step string) error) { s.fault = fault }
