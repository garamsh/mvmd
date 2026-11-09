package image

import (
	"context"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Service is what the image domain offers a driving adapter. A call returns
// nil on success, or a *machine.Error whose kind is one of its Err values.
type Service interface {
	// Pull makes the image the reference names present in the node's cache,
	// converted and verified, or leaves nothing visible. A digest already
	// present is success with no change, and pulls of one digest are one pull:
	// a call naming a digest being pulled waits for that pull's outcome. A pull
	// runs to its end, within a bound, whether or not its caller waits.
	Pull(ctx context.Context, ref Reference) error
	// Sweep removes the staging areas and the unreferenced blobs a crash left.
	// The composition site calls it once, before any other call.
	Sweep(ctx context.Context) error
}

// Settings are the node's configuration of a conversion and of the images it
// holds.
type Settings struct {
	// Platform is the one the node selects manifests for.
	Platform Platform
	// Converter names the converter, which each image records.
	Converter string
	// Resources are the converter VM's vCPUs and memory.
	Resources machine.Resources
	// OutputBytes is the size of the output file, the most an image can be.
	OutputBytes uint64
	// NameSetBytes is the most bytes of entry names the converter holds for one
	// layer.
	NameSetBytes uint64
	// Limit is how long a conversion may run, from the VMM's start.
	Limit time.Duration
}

// DefaultPullTimeout is how long a pull may run when the node names no other
// bound.
const DefaultPullTimeout = 30 * time.Minute

// Option changes how the service behaves from its defaults.
type Option func(*service)

// WithPullTimeout bounds how long a pull runs, whatever its callers' contexts
// say. A bound that is not positive is ignored.
func WithPullTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.pullTimeout = d
		}
	}
}

// pullCall is the one pull of a digest in flight. err is written before done
// is closed.
type pullCall struct {
	done chan struct{}
	err  error
}

type service struct {
	registry   Registry
	converter  Converter
	store      Store
	users      Users
	driver     machine.Driver
	supervisor machine.Supervision
	settings   Settings

	pullTimeout time.Duration
	// joined is called, when set, by each Pull that waits for a pull already
	// running. A test sets it to know a caller has joined.
	joined func()

	// publish serializes Publish: commits are applied one at a time (ADR 0007).
	publish sync.Mutex
	// mu guards pulls and is never held across a port call.
	mu    sync.Mutex
	pulls map[machine.ImageDigest]*pullCall
}

// NewService returns the image domain over the given ports. The driver and the
// supervisor are the ones the node's configuration names for conversion: the
// supervisor's expiry for a VMM is the end of the conversion limit, and the
// driver boots the converter's initrd. The caller sets the driver's Observe.
func NewService(registry Registry, converter Converter, store Store, users Users, driver machine.Driver, supervisor machine.Supervision, settings Settings, options ...Option) Service {
	s := &service{
		registry:    registry,
		converter:   converter,
		store:       store,
		users:       users,
		driver:      driver,
		supervisor:  supervisor,
		settings:    settings,
		pullTimeout: DefaultPullTimeout,
		pulls:       make(map[machine.ImageDigest]*pullCall),
	}
	for _, o := range options {
		o(s)
	}
	return s
}
