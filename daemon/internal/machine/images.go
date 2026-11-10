package machine

import "context"

func (s *service) ListImages(ctx context.Context) ([]ImageDigest, error) {
	all, err := s.images.List(ctx)
	if err != nil {
		return nil, fail(ErrFailed, "list the images: %v", err)
	}
	return all, nil
}

func (s *service) RemoveImage(ctx context.Context, image ImageDigest) error {
	if image == "" {
		return fail(ErrInvalid, "the image digest is empty")
	}
	// Create and restore hold the lock shared from locating the image until
	// they have set the Machine's record, so a removal that cannot take it now
	// is beside one that is about to reference the image. It is refused and not
	// made to wait: the image may be in use by then.
	unlock, ok := s.imageLocks.tryLockExclusive(image)
	if !ok {
		return fail(ErrPreconditionFailed, "image %s is being located by a create or restore call", image)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()

	if machine, ok := s.machineUsing(image); ok {
		return fail(ErrPreconditionFailed, "image %s is used by Machine %q", image, machine)
	}
	snapshots, err := s.snapshots.List(ctx)
	if err != nil {
		return fail(ErrFailed, "list the snapshots: %v", err)
	}
	for _, snapshot := range snapshots {
		if snapshot.Spec.Image == image {
			return fail(ErrPreconditionFailed, "image %s is used by snapshot %q; delete the snapshot first", image, snapshot.ID)
		}
	}
	if err := s.images.Remove(ctx, image); err != nil {
		return fail(ErrFailed, "remove image %s: %v", image, err)
	}
	return nil
}

// machineUsing returns the first, by identifier, of the Machines whose record
// references the image, in any state. A failed attempt's record holds no
// Machine and references nothing.
func (s *service) machineUsing(image ImageDigest) (MachineID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first MachineID
	for id, rec := range s.records {
		if rec.Machine != nil && rec.Machine.Image == image && (first == "" || id < first) {
			first = id
		}
	}
	return first, first != ""
}
