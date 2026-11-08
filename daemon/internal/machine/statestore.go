package machine

import "context"

// StateStore is mvmd's own durable record of Machines and request IDs. Loading
// it after a restart arrives with reattachment.
type StateStore interface {
	// Save durably replaces the record of one identifier.
	Save(ctx context.Context, record Record) error
	// Remove durably deletes the record of one identifier.
	Remove(ctx context.Context, id MachineID) error
}
