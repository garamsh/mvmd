package machine

import (
	"context"
	"fmt"
)

// taken is what a Create or a RestoreSnapshot has taken for a Machine that
// does not exist yet, as each step takes it. Both calls give it all back
// through giveUp at every exit that leaves no Machine, so a step added to
// either is undone by recording it here and not by a line at each exit.
type taken struct {
	id MachineID
	// slots are the holder slots of the disks the call claimed, the root
	// disk's among them (ADR 0009).
	slots []DiskID
	// uid is set once the Machine's UID is allocated.
	uid bool
	// network is set once the Machine's network is allocated.
	network bool
	// root is the root disk's file once it is made, and empty before.
	root DiskID
	// given are the disks the call made the Machine's own (ADR 0038).
	given []DiskID
	// fence is set once the Machine's own fence is made (ADR 0064).
	fence bool
}

// giveUp gives back everything the call took and returns the failure's reason,
// extended by anything that could not be given back. The order is the reverse
// of taking: the fence, the files, the network, the UID and the holder slots
// last, so no disk is free to another call while it is still the Machine's.
// It is not called for a Machine whose VMM could not be ended: what it holds
// stays held, and the caller makes the Machine Faulted, which Delete releases.
func (s *service) giveUp(ctx context.Context, t *taken, reason string) string {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	if t.fence {
		if err := s.fences.Release(ctx, t.id); err != nil {
			reason += fmt.Sprintf("; the Machine's fence could not be removed: %v", err)
		}
	}
	if err := s.reclaim(ctx, t.given...); err != nil {
		reason += fmt.Sprintf("; %v", err)
	}
	if t.root != "" {
		if err := s.disks.Remove(ctx, t.root); err != nil {
			reason += fmt.Sprintf("; the root disk could not be removed: %v", err)
		}
	}
	if t.network {
		if err := s.network.Release(ctx, t.id); err != nil {
			reason += fmt.Sprintf("; the network could not be released: %v", err)
		}
	}
	if t.uid {
		s.uids.ReleaseMachine(t.id)
	}
	s.release(t.slots...)
	*t = taken{id: t.id}
	return reason
}
