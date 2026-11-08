package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/netip"
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
	// ErrFileAbsent is a path that names nothing.
	ErrFileAbsent = errors.New("no such file")
	// ErrNotRegular is a path that resolves to something that is not a regular
	// file.
	ErrNotRegular = errors.New("not a regular file")
	// ErrPatchPrecondition is a patch whose file is at neither the base nor the
	// expected digest.
	ErrPatchPrecondition = errors.New("the file is at neither the base nor the expected digest")
)

// GuestNetwork is what a guest is told of its network: the Machine's addresses,
// a gateway for each family and the resolvers.
type GuestNetwork struct {
	Addresses []netip.Addr
	Gateways  []netip.Addr
	Resolvers []netip.Addr
}

// GuestBoot is the boot description: the filesystem UUIDs of the Machine's
// image and root disk, what to run and the network.
type GuestBoot struct {
	ImageUUID    string
	RootDiskUUID string
	Run          ExecSpec
	Network      GuestNetwork
}

// Guest is the conversation with the Guest Agent inside a Machine, over the
// channel the Driver's VMM provides. Calls other than Health reach a guest that
// is Running; the lifecycle checks that and the port does not. A call that did
// not reach the guest returns ErrGuestUnreachable.
type Guest interface {
	// Health returns nil when the agent answers.
	Health(ctx context.Context, id MachineID) error
	// Boot sends the boot description and returns when the entrypoint runs.
	// Repeating it is safe.
	Boot(ctx context.Context, id MachineID, boot GuestBoot) error
	// UpdateNetwork replaces the addresses, gateways and resolvers the guest
	// holds, as after a restore.
	UpdateNetwork(ctx context.Context, id MachineID, network GuestNetwork) error
	// Exec starts the command under the exec identifier. A repeat of an
	// identifier returns its record and runs nothing.
	Exec(ctx context.Context, id MachineID, exec ExecID, spec ExecSpec) (ExecRecord, error)
	// ReadExecOutput sends the chunks that end after the offset, in order, then
	// returns the exec's record, whose Status is ExecStarted while the command
	// runs. An offset before the guest's earliest held is an *OutputGoneError.
	// It returns ErrExecAbsent for an exec the guest does not hold.
	ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after uint64, send func(OutputChunk) error) (ExecRecord, error)
	// ReadFile sends the file's content in chunks.
	ReadFile(ctx context.Context, id MachineID, path string, send func(chunk []byte) error) error
	// WriteFile replaces the file with what content yields until io.EOF. Any
	// other error from content leaves the old content whole.
	WriteFile(ctx context.Context, id MachineID, path string, content io.Reader) error
	// PatchFile applies the patch to a file at the base digest, and leaves a
	// file at the expected digest as it is. Any other digest is
	// ErrPatchPrecondition.
	PatchFile(ctx context.Context, id MachineID, path, baseDigest, expectedDigest string, patch []byte) (PatchOutcome, error)
	// SearchFiles returns the paths under path that match the pattern.
	SearchFiles(ctx context.Context, id MachineID, path, pattern string) ([]string, error)
}
