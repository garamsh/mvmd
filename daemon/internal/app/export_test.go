package app

import "github.com/garamsh/mvmd/daemon/internal/machine"

// HoldBeforeReconcile sets the step the daemon runs before it restores and
// reconciles, for tests outside the package that hold the daemon there.
func HoldBeforeReconcile(a *App, step func()) { a.beforeReconcile = step }

// ConversionDriver is the Driver the node's converter VMs boot on.
func ConversionDriver(a *App) machine.Driver { return a.images.driver }

// PinnedBootFiles is the checker the node runs over its boot sets' files.
func PinnedBootFiles() machine.BootFileChecker { return pinnedBootFiles{} }
