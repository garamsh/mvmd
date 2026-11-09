package machine

import "context"

// StateStore is mvmd's own durable record of Machines, request IDs and the
// exec records of each Machine.
type StateStore interface {
	// Save durably replaces the record of one identifier. The record's Execs
	// are written in the same step, each replacing the entry of its exec
	// identifier; the Machine's other entries are kept.
	Save(ctx context.Context, record Record) error
	// SaveExec durably replaces one exec entry of a Machine that has a record,
	// and fails for one that has none.
	SaveExec(ctx context.Context, entry ExecEntry) error
	// Remove durably deletes the record of one identifier, with its exec
	// entries.
	Remove(ctx context.Context, id MachineID) error
	// Load reads back every record, ordered by identifier, each with all its
	// exec entries. A record that cannot be decoded is returned with
	// Unreadable set, so one bad record does not hide the others. An exec
	// entry that cannot be decoded is returned with its Machine and ID and
	// Unreadable set, and its Machine's record is read as usual. An error means
	// the store itself could not be read.
	Load(ctx context.Context) ([]Record, error)
}

// ExecEntry is what mvmd keeps of one exec: the call that made it, for its
// repeat, and the record as far as mvmd knows it.
type ExecEntry struct {
	Machine MachineID
	ID      ExecID
	Request RequestID
	Spec    ExecSpec
	Record  ExecRecord
	// Answered is whether the guest answered the call. Until it has, there is
	// no record: whether the command runs is not known.
	Answered bool
	// Unreadable is set by StateStore.Load on an entry it could not decode, to
	// the reason. Nothing else of such an entry is known but its Machine and ID.
	Unreadable string
}
