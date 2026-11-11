// Package nodecapacity is the port through which the runtime info reports what
// the node has: its CPUs, its memory and the space of its state directory's
// filesystem (ADR 0039). It is figures only; mvmd admits no call by them.
package nodecapacity

// Capacity is what the node has, as totals read when asked.
type Capacity struct {
	// CPUs is the logical CPUs mvmd's processes may run on.
	CPUs uint32
	// MemoryBytes is the host's total memory.
	MemoryBytes uint64
	// StateTotalBytes is the total size of the filesystem of mvmd's state
	// directory.
	StateTotalBytes uint64
	// StateAvailableBytes is the space of that filesystem an unprivileged
	// process may still use.
	StateAvailableBytes uint64
}
