package image

import (
	"context"
	"io"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Store is the Image Store as a pull needs it, beside the Locate the
// lifecycle uses. An image becomes visible only in Publish, and no other call
// changes what Locate answers (ADR 0007).
type Store interface {
	machine.ImageStore
	// Stage opens the working area of a pull of the image, with its output
	// file created sparse at outputBytes.
	Stage(ctx context.Context, image machine.ImageDigest, outputBytes uint64) (Staging, error)
	// PutLayer stores the layer at the index by reading content to its end,
	// and returns its path. A read error fails the call.
	PutLayer(ctx context.Context, staging Staging, index int, content io.Reader) (path string, err error)
	// Seal truncates the output to length, flushes and hashes it, enables
	// fs-verity on it and measures it, and returns the digest. The converter's
	// VMM has ended by then (ADR 0008).
	Seal(ctx context.Context, staging Staging, length uint64) (verity string, err error)
	// Record writes the MachineImage's config blob and manifest into staging.
	// It follows Seal.
	Record(ctx context.Context, staging Staging, record Record) error
	// Publish moves the staged blobs into the cache and then makes the image
	// visible in one swap of the index. It follows Record, and the caller
	// serializes its calls. An error return does not say whether the image
	// is visible: the swap of the index can have happened before a later
	// step, the sync that makes it durable, failed. Locate says which.
	Publish(ctx context.Context, staging Staging) error
	// Discard removes the staging area. What Publish moved is left.
	Discard(ctx context.Context, staging Staging) error
	// Sweep removes every staging area and every blob no listed image
	// reaches.
	Sweep(ctx context.Context) error
}
