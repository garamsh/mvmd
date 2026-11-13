package app

import (
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// NewWithClock is New with the clock the fencing deadline and the boot
// identity read, for tests that change the boot between two daemons.
func NewWithClock(cfg Config, clock fencing.Clock) (*App, error) {
	return build(cfg, func() (fencing.Clock, error) { return clock, nil })
}

// HoldBeforeReconcile sets the step the daemon runs before it restores and
// reconciles, for tests outside the package that hold the daemon there.
func HoldBeforeReconcile(a *App, step func()) { a.beforeReconcile = step }

// ConversionDriver is the Driver the node's converter VMs boot on.
func ConversionDriver(a *App) machine.Driver { return a.images.driver }

// PinnedBootFiles is the checker the node runs over its boot sets' files.
func PinnedBootFiles() machine.BootFileChecker { return pinnedBootFiles{} }
