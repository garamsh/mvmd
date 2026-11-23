package machine

// Catalog is the conversation with the node's catalog of the resources a
// Machine record names and does not own (ADR 0067). Create and restore hold a
// lease on each resource they are about to name until they have set the
// Machine's record, so the catalog cannot remove one between the two.
type Catalog interface {
	// HoldImage keeps the image from being removed until the returned release
	// is called.
	HoldImage(image ImageDigest) (release func())
}

// noCatalog is the Catalog of a lifecycle given none: nothing it names can be
// removed by anyone, so it holds nothing.
type noCatalog struct{}

func (noCatalog) HoldImage(ImageDigest) func() { return func() {} }

// WithCatalog gives the lifecycle the catalog of the resources a Machine names.
func WithCatalog(c Catalog) Option {
	return func(s *service) { s.catalog = c }
}
