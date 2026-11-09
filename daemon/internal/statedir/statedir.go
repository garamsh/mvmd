// Package statedir names the files of a Machine's state directory that the
// supervisor makes and a Driver or the supervision adapter reaches, and where
// each sits. The supervisor, the supervision adapter and every Driver take the
// names from here, so changing one side cannot leave another connecting to a
// path nothing serves (ADR 0015, ADR 0020).
//
// The layout: the state directory holds the supervisor's socket and log, the
// console socket and the run directory. The run directory is the VMM user's,
// and holds the VMM's own files and vmm.log, the supervisor's capture of the
// VMM's output. Anything mvmd must trust is beside the run directory, not in
// it.
package statedir

import "path/filepath"

const (
	supervisorSocket = "supervisor.sock"
	supervisorLog    = "supervisor.log"
	run              = "run"
	consoleSocket    = "console.sock"
	vmmLog           = "vmm.log"
)

// SupervisorSocket is the socket mvmd speaks the supervisor API on, in the
// state directory.
func SupervisorSocket(stateDir string) string {
	return filepath.Join(stateDir, supervisorSocket)
}

// SupervisorLog is what the supervisor writes about itself, in the state
// directory.
func SupervisorLog(stateDir string) string {
	return filepath.Join(stateDir, supervisorLog)
}

// ConsoleSocket is the Unix socket the supervisor serves the VMM's serial
// console on, in the state directory and outside the run directory the VMM
// owns (ADR 0020).
func ConsoleSocket(stateDir string) string {
	return filepath.Join(stateDir, consoleSocket)
}

// Run is the Machine's run directory, directly under its state directory.
func Run(stateDir string) string {
	return filepath.Join(stateDir, run)
}

// Of is the state directory a run directory sits in. It is the one place that
// relies on Run's placement, for a Driver, which is given the run directory
// alone.
func Of(runDir string) string {
	return filepath.Dir(runDir)
}

// VMMLog is the supervisor's capture of the VMM's standard output, in the run
// directory.
func VMMLog(runDir string) string {
	return filepath.Join(runDir, vmmLog)
}
