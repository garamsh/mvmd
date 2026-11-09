package cloudhypervisor

// The white-box seams: what depends on the host's architecture is read by a
// test with the architecture named, so every branch runs on any host.
var (
	Args         = args
	Cmdline      = cmdline
	CPUReadFiles = cpuReadFiles
)
