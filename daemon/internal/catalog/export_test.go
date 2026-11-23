package catalog

// ImageLockCount returns how many image leases the catalog holds or waits for now.
func ImageLockCount(s Service) int { return s.(*service).imageLocks.Held() }
