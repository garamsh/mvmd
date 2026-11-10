// Package machine is the inside of the mvmd daemon: the lifecycle of the
// Machines on one node and the invariants over them. It names no VMM, disk,
// network, registry or wire format; every crossing is a port declared here.
package machine

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
)

// MachineID is the client-chosen identifier of a Machine.
type MachineID string

// RequestID is the client-chosen identifier of one creating call.
type RequestID string

// DiskID identifies a disk the node holds.
type DiskID string

// ImageDigest names an image on the node by its digest, never by a tag.
type ImageDigest string

// DiskOwner names the Machine that owns a disk and the UID and GID of its VMM
// user (ADR 0015).
type DiskOwner struct {
	ID  MachineID
	UID uint32
	GID uint32
}

// UIDRange names a contiguous range of UIDs (and GIDs) the node reserves
// for its Machines (ADR 0015).
type UIDRange struct {
	First uint32
	Count uint32
}

// Validate returns an error if the range is unusable: First is root, Count is
// empty, or the top of the range overflows uint32.
func (r UIDRange) Validate() error {
	switch {
	case r.First == 0:
		return errors.New("UID range must not include root (UID 0)")
	case r.Count == 0:
		return errors.New("UID range must have at least one entry")
	}
	// Check that First+Count-1 does not overflow MaxUint32.
	if r.Count > 0 && r.First > ^uint32(0)-(r.Count-1) {
		return errors.New("UID range overflows uint32")
	}
	return nil
}

// Contains returns true if uid falls within the range [First, First+Count).
// It uses subtraction to avoid the overflow that a direct sum would cause.
func (r UIDRange) Contains(uid uint32) bool {
	return uid >= r.First && uid-r.First < r.Count
}

// User is the UID and GID of a Machine's VMM user, always equal (ADR 0015).
type User struct {
	UID uint32
	GID uint32
}

// State is where a Machine is in its lifecycle.
type State int

const (
	// StateCreated is configured on the node and has no VMM process.
	StateCreated State = iota
	// StateRunning is booted; the guest executes.
	StateRunning
	// StatePaused is stopped with memory held.
	StatePaused
	// StateStopped is not executing; disks are retained.
	StateStopped
	// StateFaulted is a Machine whose state mvmd cannot establish.
	StateFaulted
)

// String returns the state's name as runtime-api.md spells it.
func (s State) String() string {
	switch s {
	case StateCreated:
		return "Created"
	case StateRunning:
		return "Running"
	case StatePaused:
		return "Paused"
	case StateStopped:
		return "Stopped"
	case StateFaulted:
		return "Faulted"
	default:
		return fmt.Sprintf("State(%d)", int(s))
	}
}

// Resources is an absolute target for a Machine's CPU and memory.
type Resources struct {
	VCPUs       uint32
	MemoryBytes uint64
}

// MemoryAlignment is the unit a memory figure is a multiple of: the step a
// hot-plugged memory device takes. The lifecycle refuses any other figure for a
// Machine's resources or ceiling (ADR 0029).
const MemoryAlignment = 2 << 20

// OrElse returns r with each figure that is zero taken from other.
func (r Resources) OrElse(other Resources) Resources {
	if r.VCPUs == 0 {
		r.VCPUs = other.VCPUs
	}
	if r.MemoryBytes == 0 {
		r.MemoryBytes = other.MemoryBytes
	}
	return r
}

// within reports whether each figure of r is at most that of ceiling.
func (r Resources) within(ceiling Resources) bool {
	return r.VCPUs <= ceiling.VCPUs && r.MemoryBytes <= ceiling.MemoryBytes
}

// DiskAttachment is a disk attached to a Machine and how it is attached.
type DiskAttachment struct {
	Disk     DiskID
	ReadOnly bool
}

// Spec is what Create Machine carries, and what a Machine is configured with.
type Spec struct {
	Image     ImageDigest
	Resources Resources
	// Ceiling is the most a live resize of the Machine reaches, and the most
	// the guest can have (ADR 0029). A figure Create is given as zero is the
	// Machine's resources; a Machine holds every figure set, and none below
	// its resources.
	Ceiling       Resources
	RootDiskBytes uint64
	Disks         []DiskAttachment
	Policy        NetworkPolicy
}

// Machine is a Machine as Inspect reports it.
type Machine struct {
	ID    MachineID
	State State
	Spec
	// RootDisk is the disk Create Machine made for this Machine.
	RootDisk DiskID
	// RootDiskUUID is the ext4 filesystem UUID of the root disk, sent to
	// the Guest Agent over the channel (ADR 0010).
	RootDiskUUID string
	// BootMemoryBytes is the memory the Machine's VMM booted with, the least a
	// live resize reaches (ADR 0029). It is set at each boot and restore, and is
	// meaningful while the Machine is Running or Paused.
	BootMemoryBytes uint64
	// User is the UID and GID allocated for this Machine's VMM (ADR 0015).
	User User
	// Network is the identity mvmd assigned for the Machine's life.
	Network NetworkIdentity
	// FailureReason is the reason for the last failure, empty if none.
	FailureReason string
	// Driver is the Driver in service, set by Inspect.
	Driver DriverInfo
}

// Request is the record of a creating call, kept for its repeat.
type Request struct {
	ID RequestID
	// Snapshot is the snapshot a restoring call named, empty for Create. A
	// restoring call's Spec holds only its network policy: the rest of the
	// Machine comes from the snapshot.
	Snapshot SnapshotID
	Spec     Spec
	// Failure is the original outcome when the call failed, nil on success.
	Failure *Error
}

// Record is mvmd's durable record of one identifier: the Machine, if one
// exists, and the creating call that named it.
type Record struct {
	ID      MachineID
	Machine *Machine
	Request Request
	// Execs are the Machine's exec entries: those a Save writes with the
	// record, and all of them in what Load returns.
	Execs []ExecEntry
	// Unreadable is set by StateStore.Load on a record it could not decode, to
	// the reason. Nothing else of such a record is known.
	Unreadable string
}

// The outcome kinds of runtime-api.md §Calling rules. Success is a nil error.
var (
	ErrInvalid            = errors.New("rejected as invalid")
	ErrNotFound           = errors.New("not found")
	ErrConflict           = errors.New("conflict")
	ErrPreconditionFailed = errors.New("precondition failed")
	ErrUnsupported        = errors.New("unsupported")
	ErrFailed             = errors.New("failed")
)

// Error is a call's non-success outcome: its kind and the reason for it.
// errors.Is reaches the kind; errors.As reaches the reason.
type Error struct {
	Kind   error
	Reason string
}

func (e *Error) Error() string { return e.Kind.Error() + ": " + e.Reason }

// Unwrap returns the outcome kind.
func (e *Error) Unwrap() error { return e.Kind }

// NewError returns an outcome of the kind with its reason, for the other
// domains of the inside that answer calls in these terms.
func NewError(kind error, format string, args ...any) *Error {
	return fail(kind, format, args...)
}

func fail(kind error, format string, args ...any) *Error {
	return &Error{Kind: kind, Reason: fmt.Sprintf(format, args...)}
}

func (m Machine) clone() Machine {
	m.Spec = m.Spec.clone()
	m.Network.Addresses = slices.Clone(m.Network.Addresses)
	m.Network.Gateways = slices.Clone(m.Network.Gateways)
	m.Network.Resolvers = slices.Clone(m.Network.Resolvers)
	return m
}

func (s Spec) clone() Spec {
	s.Disks = slices.Clone(s.Disks)
	s.Policy = s.Policy.clone()
	return s
}

func (s Spec) equal(o Spec) bool {
	return reflect.DeepEqual(s.clone(), o.clone())
}

func (m Machine) attachment(disk DiskID) (DiskAttachment, bool) {
	for _, a := range m.Disks {
		if a.Disk == disk {
			return a, true
		}
	}
	return DiskAttachment{}, false
}
