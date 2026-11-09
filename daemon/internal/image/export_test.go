package image

// WhenJoined makes a service call f for each Pull that waits for a pull
// already running, so a test releases the pull only once its callers joined.
func WhenJoined(f func()) Option {
	return func(s *service) { s.joined = f }
}
