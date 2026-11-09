package cloudhypervisor

import "io"

// The white-box seams: what depends on the host's architecture is read by a
// test with the architecture named, so every branch runs on any host.
var (
	Args         = args
	Cmdline      = cmdline
	CPUReadFiles = cpuReadFiles
)

// CPUModel names the model of the first processor in a /proc/cpuinfo.
func CPUModel(r io.Reader) (string, error) { return cpuModel(r) }
