package fencing

import "context"

// DeadlineStore is mvmd's durable record of the deadline.
type DeadlineStore interface {
	// Load returns the record, and false when none was ever saved.
	Load(ctx context.Context) (Record, bool, error)
	// Save durably replaces the record; it returns once the record is durable.
	Save(ctx context.Context, record Record) error
}
