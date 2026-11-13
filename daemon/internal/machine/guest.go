package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"
)

// ExecID is the client-chosen identifier of an exec record.
type ExecID string

// ExecSpec is the command an exec starts.
type ExecSpec struct {
	Command    []string
	Env        map[string]string
	WorkingDir string
}

// ExecStatus is where an exec record is.
type ExecStatus int

const (
	// ExecNotStarted is a command that did not start; Reason says why.
	ExecNotStarted ExecStatus = iota
	// ExecStarted is a command that runs, as far as mvmd knows.
	ExecStarted
	// ExecExited is a command that ended; ExitCode or TermSignal says how.
	ExecExited
	// ExecCutShort is a command whose Machine stopped while it ran.
	ExecCutShort
)

// ExecRecord is what mvmd knows of one command.
type ExecRecord struct {
	ID     ExecID
	Status ExecStatus
	// Reason is why the command did not start or was cut short.
	Reason string
	// ExitCode is the exit code of an exited command that was not killed by a
	// signal.
	ExitCode int32
	// TermSignal is the signal that ended an exited command, or zero.
	TermSignal int32
	// EndedAt is when an exited command ended by the guest's clock, as the
	// Guest Agent read it when it reaped the command. It is zero where the
	// guest gave no time; mvmd never puts its own in its place.
	EndedAt time.Time
}

// OutputStream names which of the command's streams a chunk came from.
type OutputStream int

const (
	OutputStdout OutputStream = iota + 1
	OutputStderr
)

// OutputChunk is output of a command. Offset is the position of its first
// byte in the command's output as one sequence, stdout and stderr in the order
// they were received.
type OutputChunk struct {
	Stream OutputStream
	Offset uint64
	Data   []byte
}

// OutputGoneError is what reading exec output returns for an offset older than
// the output the guest still holds. The client resumes from Earliest.
type OutputGoneError struct{ Earliest uint64 }

func (e *OutputGoneError) Error() string {
	return fmt.Sprintf("the output before offset %d is no longer held; resume from %d", e.Earliest, e.Earliest)
}

// BootStepError is what Guest.Boot returns when the agent answered and one
// step of the boot failed. The guest stays up, refusing every call but a
// repeat of Boot (guest.md), so it is a different outcome from a call that did
// not reach the agent.
type BootStepError struct {
	Step   string
	Detail string
}

func (e *BootStepError) Error() string {
	return fmt.Sprintf("guest boot step %q failed: %s", e.Step, e.Detail)
}

// EntryKind is what a directory entry is.
type EntryKind int

const (
	EntryFile EntryKind = iota + 1
	EntryDirectory
	EntrySymlink
	EntryOther
)

// FileEntry is one entry of a guest directory. Size is in bytes.
type FileEntry struct {
	Path string
	Kind EntryKind
	Size uint64
}

// PatchOutcome is what a patch did.
type PatchOutcome int

const (
	// PatchApplied is a patch applied to a file at its base digest.
	PatchApplied PatchOutcome = iota + 1
	// PatchAlreadyApplied is a file already at the expected digest, left as it
	// is.
	PatchAlreadyApplied
)

// The errors a Guest returns, which the lifecycle maps to outcome kinds.
var (
	// ErrGuestUnreachable is a call that did not reach the Guest Agent.
	ErrGuestUnreachable = errors.New("the Guest Agent is not reachable")
	// ErrExecAbsent is an exec the guest holds no record of.
	ErrExecAbsent = errors.New("the guest has no such exec")
	// ErrExecNotRunning is a signal to a command that has ended.
	ErrExecNotRunning = errors.New("the exec is not running")
	// ErrSignalInvalid is a signal the guest does not take: a number that is
	// not a signal.
	ErrSignalInvalid = errors.New("not a signal number")
	// ErrPatternInvalid is a Search pattern the guest refuses because it is
	// not a valid glob; the reason names it.
	ErrPatternInvalid = errors.New("not a valid glob pattern")
	// ErrNotDirectory is a path that resolves to something that is not a
	// directory, where one is listed.
	ErrNotDirectory = errors.New("not a directory")
	// ErrFileAbsent is a path that names nothing.
	ErrFileAbsent = errors.New("no such file")
	// ErrNotRegular is a path that resolves to something that is not a regular
	// file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrPatchPrecondition is a patch whose file is at neither the base nor the
	// expected digest.
	ErrPatchPrecondition = errors.New("the file is at neither the base nor the expected digest")
	// ErrDiskInUse is a disk whose device the guest has mounted or holds, so
	// it is not released.
	ErrDiskInUse = errors.New("the guest is using the disk")
	// ErrDiskUnknown is a disk the guest never found. A disk it found and
	// released is not unknown.
	ErrDiskUnknown = errors.New("the guest has no record of the disk")
)

// GuestNetwork is what a guest is told of its network: the Machine's addresses,
// a gateway for each family and the resolvers.
type GuestNetwork struct {
	Addresses []netip.Addr
	Gateways  []netip.Addr
	Resolvers []netip.Addr
}

// CatchUp is what a restored guest is told (ADR 0016): the restore it is
// told of, fresh entropy to reseed from, the host's wall clock, and its new
// network, in that order.
type CatchUp struct {
	// RestoreID is the request ID of the restore, unique to it.
	RestoreID RequestID
	// Entropy is cryptographically random bytes drawn afresh for this restore.
	Entropy []byte
	// HostTime is the host's wall clock when the restore sent this.
	HostTime time.Time
	Network  GuestNetwork
}

// WallClock is the host's wall clock, which a restored guest is set to.
type WallClock interface {
	Now() time.Time
}

// GuestBoot is the boot description: the filesystem UUIDs of the Machine's
// image and root disk, what to run and the network.
type GuestBoot struct {
	ImageUUID    string
	RootDiskUUID string
	Run          ExecSpec
	// User is the user the entrypoint runs as, in the OCI form; empty is root.
	User string
	// Files are placed in the guest before the entrypoint runs. Their content
	// may be secret.
	Files   []PlacedFile
	Network GuestNetwork
	// Clock is the host's wall clock. The Guest reads it as it sends the
	// request, after the agent answers and not before, because the agent
	// listens some time after the VMM starts, and the guest sets its clock to
	// the reading (ADR 0044). A nil Clock sends no time and leaves the guest's
	// clock as its kernel set it.
	Clock WallClock
}

// GuestSize is what a guest reports of its size: the vCPUs online and the
// memory its kernel holds, in bytes.
type GuestSize struct {
	VCPUs       uint32
	MemoryBytes uint64
}

// Guest is the conversation with the Guest Agent inside a Machine, over the
// channel the Driver's VMM provides. Calls other than Health reach a guest that
// is Running; the lifecycle checks that and the port does not. A call that did
// not reach the guest returns ErrGuestUnreachable.
type Guest interface {
	// Health returns nil when the agent answers.
	Health(ctx context.Context, id MachineID) error
	// Boot sends the boot description and returns when the entrypoint runs.
	// Repeating it is safe, and a repeat does not move the guest's clock
	// again.
	Boot(ctx context.Context, id MachineID, boot GuestBoot) error
	// CatchUp brings a restored guest up to date and returns when it has caught
	// up. Repeating it with the same restore ID is safe (ADR 0016).
	CatchUp(ctx context.Context, id MachineID, catchUp CatchUp) error
	// Exec starts the command under the exec identifier. A repeat of an
	// identifier returns its record and runs nothing.
	Exec(ctx context.Context, id MachineID, exec ExecID, spec ExecSpec) (ExecRecord, error)
	// ReadExecOutput sends the chunks that end after the offset, in order, then
	// returns the exec's record, whose Status is ExecStarted while the command
	// runs. An offset before the guest's earliest held is an *OutputGoneError.
	// It returns ErrExecAbsent for an exec the guest does not hold.
	ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after uint64, send func(OutputChunk) error) (ExecRecord, error)
	// Signal delivers the signal, a number as on Linux, to the exec's command.
	// It returns ErrExecAbsent for an exec the guest does not hold,
	// ErrExecNotRunning for a command that has ended and ErrSignalInvalid for a
	// number that is not a signal.
	Signal(ctx context.Context, id MachineID, exec ExecID, signal int32) error
	// ReadFile sends the file's content in chunks.
	ReadFile(ctx context.Context, id MachineID, path string, send func(chunk []byte) error) error
	// WriteFile replaces the file with what content yields until io.EOF. Any
	// other error from content leaves the old content whole.
	WriteFile(ctx context.Context, id MachineID, path string, content io.Reader) error
	// PatchFile applies the patch to a file at the base digest, and leaves a
	// file at the expected digest as it is. Any other digest is
	// ErrPatchPrecondition.
	PatchFile(ctx context.Context, id MachineID, path, baseDigest, expectedDigest string, patch []byte) (PatchOutcome, error)
	// ListFiles sends the entries of the directory in runs, one run to a
	// message of the guest's, sorted by path across the runs. send is not
	// called with an empty run.
	ListFiles(ctx context.Context, id MachineID, path string, send func(entries []FileEntry) error) error
	// SearchFiles sends the paths under path that match the pattern, in runs
	// sorted across the runs, as ListFiles does. The pattern is a glob
	// (runtime-api.md §Guest access); one the guest refuses is
	// ErrPatternInvalid.
	SearchFiles(ctx context.Context, id MachineID, path, pattern string, send func(paths []string) error) error
	// Terminate asks the agent to apply its Termination rule (guest.md): send
	// SIGTERM to the entrypoint, or end the Machine when none runs. It returns
	// when the agent has taken the request, not when the Machine has ended, and
	// it is the one call a guest that has not booted also serves. Repeating it
	// is safe.
	Terminate(ctx context.Context, id MachineID) error
	// FindDisk asks the agent to find the block device of a disk the Driver has
	// just hot-plugged and to record it under the disk's ID (ADR 0022). It is
	// served before the guest has booted. A failure means the guest did not
	// find exactly one new device. Repeating it for a disk it holds is safe, and
	// for a disk it released it rescans, as for a new one. A non-empty MountPath
	// has the agent mount the disk there (ADR 0053); a repeat carries the same
	// MountPath and ReadOnly.
	FindDisk(ctx context.Context, id MachineID, a DiskAttachment) error
	// GrowDisk asks the agent to read the device size of a disk it found and
	// to grow the ext4 filesystem mounted from it to that size (ADR 0050,
	// 0055), and returns the device size the guest sees. A disk with no mount
	// only has its size read. Repeating it grows nothing, so it is safe, and a
	// size below the one the Driver was told means the guest has not yet seen
	// the change: the caller asks again. A disk the agent never found or has
	// released is ErrDiskUnknown.
	GrowDisk(ctx context.Context, id MachineID, disk DiskID) (uint64, error)
	// OnlineVcpus asks the agent to bring online the vCPUs the Driver has just
	// hot-plugged, up to vcpus, and returns how many are online and the memory
	// the guest holds (ADR 0029). It onlines only a CPU that is present and
	// offline, so a count the VMM has not yet presented comes back lower than
	// asked, which is not an error: the caller asks again. It is served before
	// the guest has booted. A vcpus of zero is refused. Repeating it is safe
	// and returns the same counts.
	OnlineVcpus(ctx context.Context, id MachineID, vcpus uint32) (GuestSize, error)
	// ReleaseDisk asks the agent to release the device it recorded for the
	// disk, so the Driver may unplug it. It returns ErrDiskInUse when the
	// device is mounted or held, and ErrDiskUnknown when the agent never found
	// the disk. A disk it already released is released again without effect, so
	// a repeat after a release succeeds.
	ReleaseDisk(ctx context.Context, id MachineID, disk DiskID) error
}
