package memory

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// StartPermitter is an in-memory machine.StartPermitter that permits every
// start until its step "permit" is told to fail.
type StartPermitter struct {
	Failures
}

// NewStartPermitter returns a StartPermitter that permits.
func NewStartPermitter() *StartPermitter { return &StartPermitter{} }

// PermitStart implements machine.StartPermitter.
func (p *StartPermitter) PermitStart(_ context.Context, _ machine.MachineID) error {
	return p.Enter("permit")
}
