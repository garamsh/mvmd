package app

// HoldBeforeReconcile sets the step the daemon runs before it restores and
// reconciles, for tests outside the package that hold the daemon there.
func HoldBeforeReconcile(a *App, step func()) { a.beforeReconcile = step }
