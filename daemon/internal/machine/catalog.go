package machine

import "context"

// Catalog is the conversation with the node's catalog of the resources a
// Machine record names and does not own (ADR 0067). Create and restore hold a
// lease on each resource they are about to name until they have set the
// Machine's record, so the catalog cannot remove one between the two.
type Catalog interface {
	// HoldImage keeps the image from being removed until the returned release
	// is called.
	HoldImage(image ImageDigest) (release func())
	// HoldBootSet keeps the boot set of the name from being removed until the
	// returned release is called.
	HoldBootSet(name string) (release func())
	// HoldDiskSnapshot keeps the disk snapshot from being removed, and from being
	// held by a call of the other kind, until the returned release is called:
	// exclusive for the call that writes it, shared for one that reads it.
	HoldDiskSnapshot(snapshot DiskSnapshotID, exclusive bool) (release func())
	// ResolveBootSet finds the set the name offers, a configured one before an
	// added one, or returns ErrBootSetAbsent.
	ResolveBootSet(ctx context.Context, name string) (BootSet, error)
}

// noCatalog is the Catalog of a lifecycle given none: nothing it names can be
// removed by anyone, so it holds nothing.
type noCatalog struct{}

func (noCatalog) HoldImage(ImageDigest) func() { return func() {} }

func (noCatalog) HoldBootSet(string) func() { return func() {} }

func (noCatalog) HoldDiskSnapshot(DiskSnapshotID, bool) func() { return func() {} }

func (noCatalog) ResolveBootSet(context.Context, string) (BootSet, error) {
	return BootSet{}, ErrBootSetAbsent
}

// WithCatalog gives the lifecycle the catalog of the resources a Machine names.
func WithCatalog(c Catalog) Option {
	return func(s *service) { s.catalog = c }
}
