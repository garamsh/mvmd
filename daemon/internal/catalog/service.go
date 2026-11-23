// Package catalog is the node's catalog of the resources a Machine record
// names and does not own: the images it was made from, the boot sets and the
// disk snapshots (ADR 0067). Listing and removing one
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
	// AddBootSet reads the two files of the upload into the node's store under
	// the name (ADR 0063). A name that is offered with the same digests is
	// success with no change, with other digests ErrConflict; a file whose
	// contents do not have its digest is ErrInvalid, naming it.
	AddBootSet(ctx context.Context, name string, upload machine.BootUpload) (machine.BootSetInfo, error)
	// ListBootSets returns the configured and the added boot sets, ordered by
	// name.
	ListBootSets(ctx context.Context) ([]machine.BootSetInfo, error)
	// RemoveBootSet removes an added boot set. A configured one, one a Machine's
	// record names and one a Create call is resolving are
	// ErrPreconditionFailed; an absent name is success.
	RemoveBootSet(ctx context.Context, name string) error
	// ListDiskSnapshots returns every disk snapshot the node holds, ordered by
	// identifier.
	ListDiskSnapshots(ctx context.Context) ([]machine.DiskSnapshotInfo, error)
	// DeleteDiskSnapshot removes the disk snapshot. An absent one succeeds; one a
	// SnapshotDisk or CreateDiskFromSnapshot call is using is refused with
	// ErrPreconditionFailed. The disk it was taken of is not touched.
	DeleteDiskSnapshot(ctx context.Context, snapshot machine.DiskSnapshotID) error
}

// Option changes how the catalog behaves from its defaults.
type Option func(*service)

// WithBootSets sets the boot sets the node's configuration names (ADR 0058).
// Without it the node configures none.
func WithBootSets(sets machine.BootSets) Option {
	return func(s *service) { s.bootSets = sets }
}

// WithDiskSnapshots sets the store of disk snapshots (ADR 0056). Without it the
// node offers none, and the calls on them are ErrUnsupported.
func WithDiskSnapshots(d machine.DiskSnapshotStore) Option {
	return func(s *service) { s.diskSnaps = d }
}

// WithAddedBootSets sets the store of the boot sets a client adds (ADR 0063).
// Without it the node keeps none, and Add boot set is ErrUnsupported.
func WithAddedBootSets(b machine.BootSetStore) Option {
	return func(s *service) { s.addedSets = b }
}

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
	// bootSets are the boot sets the node's configuration names, and addedSets
	// the store of the ones a client added, or nil where the node keeps none.
	bootSets  machine.BootSets
	addedSets machine.BootSetStore
	// bootSetLocks serializes the removal of a boot set against the Create
	// calls that resolve it: they hold it shared from resolving the name until
	// the Machine's record is set (ADR 0063).
	bootSetLocks keyedlock.Locks[string]
	// diskSnaps are the disk snapshots the node holds, or nil where it offers none.
	diskSnaps machine.DiskSnapshotStore
	// diskSnapLocks serializes SnapshotDisk, exclusive, against the Create disk
	// calls that read the same disk snapshot, shared (ADR 0056).
	diskSnapLocks keyedlock.Locks[machine.DiskSnapshotID]
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
