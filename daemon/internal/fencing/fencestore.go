package fencing

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// FenceStore is mvmd's durable record of each fenced Machine's own fence. A
// record that was never renewed has no expiry and no renewal number: the
// Machine is enrolled and its fence not armed.
type FenceStore interface {
	// Load returns every record.
	Load(ctx context.Context) (map[machine.MachineID]Record, error)
	// Save durably replaces the Machine's record; it returns once the record
	// is durable.
	Save(ctx context.Context, id machine.MachineID, record Record) error
	// Delete removes the Machine's record. It succeeds when none exists.
	Delete(ctx context.Context, id machine.MachineID) error
}
