package firecracker

// The white-box seams: what depends on the host's architecture is read by a
// test with the architecture named, so every branch runs on any host.
var (
	BootArgs     = bootArgs
	CPUReadFiles = cpuReadFiles
)
