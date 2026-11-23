// Package catalog is the node's catalog of the resources a Machine record
// names and does not own: the images it was made from, today, and the boot sets
// and the disk snapshots as they move here (ADR 0067). Listing and removing one
// is the catalog's call, with a lock of its own; the lifecycle takes a lease on
// the one it is about to name through the machine.Catalog port.
//
// The catalog never calls the lifecycle. It reads the Machine records from the
// State Store, so a lock it holds is held across no Machine's lock, and a lease
// the lifecycle holds can be waited for by no one but the lifecycle's own calls.
package catalog

import (
	"context"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/keyedlock"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Service is what the catalog offers a driving adapter. A call returns nil on
// success, or a *machine.Error whose kind is one of its Err values. A Service is
// also the machine.Catalog the lifecycle takes its leases from.
type Service interface {
	machine.Catalog
	// ListImages returns every image the node holds, ordered by digest.
	ListImages(ctx context.Context) ([]machine.ImageInfo, error)
	// RemoveImage unlists the image and reclaims its blobs. An absent image
	// succeeds. An image a Machine record references, a snapshot references,
	// or a Create or restore call is locating is refused with
	// ErrPreconditionFailed.
	RemoveImage(ctx context.Context, image machine.ImageDigest) error
}

// Option changes how the catalog behaves from its defaults.
type Option func(*service)

// WithCallTimeout bounds how long a removal runs, whatever the caller's context
// says. A bound that is not positive is ignored.
func WithCallTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.callTimeout = d
		}
	}
}

type service struct {
	images    machine.ImageStore
	snapshots machine.SnapshotStore
	store     machine.StateStore
	// callTimeout bounds an accepted call, which does not end with its caller's
	// context (runtime-api.md §Calling rules).
	callTimeout time.Duration
	// imageLocks serializes the removal of an image against the Create and
	// restore calls that locate it: they hold it shared from Locate until the
	// Machine's record is set.
	imageLocks keyedlock.Locks[machine.ImageDigest]
}

// NewService returns the catalog over the node's image cache and snapshots,
// and the State Store, which holds the Machine records that reference them.
func NewService(images machine.ImageStore, snapshots machine.SnapshotStore, store machine.StateStore, options ...Option) Service {
	s := &service{images: images, snapshots: snapshots, store: store, callTimeout: machine.DefaultCallTimeout}
	for _, o := range options {
		o(s)
	}
	return s
}

// accepted returns the context a removal runs on: the caller's values, and its
// own bound. The caller's disconnect or deadline does not end it.
func (s *service) accepted(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), s.callTimeout)
}

// HoldImage implements machine.Catalog.
func (s *service) HoldImage(image machine.ImageDigest) func() {
	return s.imageLocks.Lock(image, false)
}
