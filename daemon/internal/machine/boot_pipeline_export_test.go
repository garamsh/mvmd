package machine

import "context"

// BootPlan is the part of a boot pipeline's plan that a test chooses; the
// image's UUID is of no use to it.
type BootPlan struct {
	Describe func(ctx context.Context, boot BootSpec) (ProcessDescription, error)
	VMM      func(ctx context.Context, boot BootSpec) error
	Guest    func(ctx context.Context) error
}

// BootOutcome is what a boot pipeline reports to its caller.
type BootOutcome struct {
	Failure     *Error
	VMM         bool
	EndErr      error
	WithdrawErr error
	Reason      string
}

// BringUp runs the boot pipeline for the Machine as Start and RestoreSnapshot
// do, with no Machine state or record around it.
func BringUp(ctx context.Context, s Service, m Machine, p BootPlan) BootOutcome {
	res := s.(*service).bringUp(ctx, bootPlan{
		machine:  m,
		describe: p.Describe,
		vmm:      p.VMM,
		guest: func(ctx context.Context, _ LocatedImage) error {
			return p.Guest(ctx)
		},
		guestWhile: "booting",
	})
	out := BootOutcome{Failure: res.failure, VMM: res.begun.vmm, EndErr: res.endErr, WithdrawErr: res.withdrawErr}
	if res.failure != nil {
		out.Reason = res.reason()
	}
	return out
}
