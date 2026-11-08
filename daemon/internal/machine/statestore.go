package machine

import "context"

// StateStore is mvmd's own durable record of Machines and request IDs.
type StateStore interface {
	// Save durably replaces the record of one identifier.
	Save(ctx context.Context, record Record) error
	// Remove durably deletes the record of one identifier.
	Remove(ctx context.Context, id MachineID) error
	// Load reads back every record, ordered by identifier. A record that
	// cannot be decoded is returned with Unreadable set, so one bad record
	// does not hide the others; an error means the store itself could not be
	// read.
	Load(ctx context.Context) ([]Record, error)
}
