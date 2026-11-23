package cloudhypervisor

import "github.com/garamsh/mvmd/daemon/internal/vmmboot"

// The white-box seams: what depends on the host's architecture is read by a
// test with the architecture named, so every branch runs on any host.
var (
	Args         = args
	Cmdline      = cmdline
	OSCmdline    = osCmdline
	CPUReadFiles = vmmboot.CPUCacheFiles
)

// SetGOARCH names the architecture the Driver runs its VMM on, so a test runs
// either branch on any host.
func SetGOARCH(d *Driver, goarch string) { d.goarch = goarch }
