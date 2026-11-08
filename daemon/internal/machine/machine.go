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

// DiskAttachment is a disk attached to a Machine and how it is attached.
type DiskAttachment struct {
	Disk     DiskID
	ReadOnly bool
}

// Spec is what Create Machine carries, and what a Machine is configured with.
type Spec struct {
	Image         ImageDigest
	Resources     Resources
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
	// Network is the identity mvmd assigned for the Machine's life.
	Network NetworkIdentity
	// FailureReason is the reason for the last failure, empty if none.
	FailureReason string
	// Driver is the Driver in service, set by Inspect.
	Driver DriverInfo
}

// Request is the record of a creating call, kept for its repeat.
type Request struct {
	ID   RequestID
	Spec Spec
	// Failure is the original outcome when the call failed, nil on success.
	Failure *Error
}

// Record is mvmd's durable record of one identifier: the Machine, if one
// exists, and the creating call that named it.
type Record struct {
	ID      MachineID
	Machine *Machine
	Request Request
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
