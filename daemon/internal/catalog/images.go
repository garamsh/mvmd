package catalog

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func (s *service) ListImages(ctx context.Context) ([]machine.ImageInfo, error) {
	all, err := s.images.List(ctx)
	if err != nil {
		return nil, fail(machine.ErrFailed, "list the images: %v", err)
	}
	return all, nil
}

func (s *service) RemoveImage(ctx context.Context, image machine.ImageDigest) error {
	if image == "" {
		return fail(machine.ErrInvalid, "the image digest is empty")
	}
	// Create and restore hold the lease shared from locating the image until
	// they have set the Machine's record, so a removal that cannot take it now
	// is beside one that is about to reference the image. It is refused and not
	// made to wait: the image may be in use by then.
	unlock, ok := s.imageLocks.TryLockExclusive(image)
	if !ok {
		return fail(machine.ErrPreconditionFailed, "image %s is being located by a create or restore call", image)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx)
	defer cancel()

	records, err := s.store.Load(ctx)
	if err != nil {
		return fail(machine.ErrFailed, "read the Machine records: %v", err)
	}
	if id, ok := machineUsing(records, image); ok {
		return fail(machine.ErrPreconditionFailed, "image %s is used by Machine %q", image, id)
	}
	snapshots, err := s.snapshots.List(ctx)
	if err != nil {
		return fail(machine.ErrFailed, "list the snapshots: %v", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Spec.Image == image {
			return fail(machine.ErrPreconditionFailed, "image %s is used by snapshot %q; delete the snapshot first", image, snapshot.ID)
		}
	}
	if err := s.images.Remove(ctx, image); err != nil {
		return fail(machine.ErrFailed, "remove image %s: %v", image, err)
	}
	return nil
}

// machineUsing returns the first, by identifier, of the Machines whose record
// references the image, in any state. A failed attempt's record holds no
// Machine and references nothing, and a record that could not be read holds
// none either, as the lifecycle reads it.
func machineUsing(records []machine.Record, image machine.ImageDigest) (machine.MachineID, bool) {
	var first machine.MachineID
	for _, rec := range records {
		if rec.Machine != nil && rec.Unreadable == "" && rec.Machine.Image == image && (first == "" || rec.ID < first) {
			first = rec.ID
		}
	}
	return first, first != ""
}

func fail(kind error, format string, args ...any) *machine.Error {
	return machine.NewError(kind, format, args...)
}
