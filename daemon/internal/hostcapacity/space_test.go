package hostcapacity

import (
	"testing"

	"golang.org/x/sys/unix"
)

// The host test cannot tell f_bavail from f_bfree on a filesystem that reserves
// no blocks for root (tmpfs, CI's temporary directory), so this one feeds space
// a Statfs_t whose two differ.
func TestSpace_TakesTheAvailableBytesFromBavailAndNotBfree(t *testing.T) {
	fs := unix.Statfs_t{Frsize: 4096, Blocks: 1000, Bfree: 300, Bavail: 200}

	total, available := space(fs)

	if want := uint64(1000 * 4096); total != want {
		t.Errorf("total = %d, want %d (Blocks * Frsize)", total, want)
	}
	if want := uint64(200 * 4096); available != want {
		t.Errorf("available = %d, want %d (Bavail * Frsize; Bfree would give %d)", available, want, 300*4096)
	}
}
