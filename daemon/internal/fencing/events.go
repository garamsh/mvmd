package fencing

// Event is a change of the deadline.
type Event int

const (
	// EventArmed is a deadline that was not armed becoming armed: by a renewal
	// after a boot, an expiry, or no record, or by the reconciliation that
	// found a recorded expiry still in the future.
	EventArmed Event = iota + 1
	// EventRenewed is a renewal applied while the deadline was armed.
	EventRenewed
	// EventExpired is the expiry passing, once every supervisor is gone.
	EventExpired
)

// Events is the deadline's record of its changes. The deadline calls it from
// the one place it commits a change.
type Events interface {
	Deadline(event Event)
}
