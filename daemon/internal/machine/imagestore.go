package machine

import (
	"context"
	"errors"
)

// ErrImageAbsent is what ImageStore.Locate returns for an image the node does
// not hold.
var ErrImageAbsent = errors.New("image not on the node")

// LocatedImage is what the node holds of an image: its read-only filesystem
// file, that filesystem's UUID, and the configuration the image says to run
// with.
type LocatedImage struct {
	// Path is the image's read-only filesystem file.
	Path string
	// UUID is the UUID the image's filesystem carries, by which the guest
	// finds the device (ADR 0010).
	UUID string
	// Run is what the guest runs: the image's entrypoint and command as one
	// argument vector, its environment and its working directory.
	Run ExecSpec
}

// ImageStore is the conversation with the node's image cache.
type ImageStore interface {
	// Locate returns what the node holds of the image, or ErrImageAbsent.
	Locate(ctx context.Context, image ImageDigest) (LocatedImage, error)
	// List returns the digest of every image the node holds, ordered.
	List(ctx context.Context) ([]ImageDigest, error)
	// Remove unlists the image and reclaims the blobs only it reached. An
	// absent image succeeds. A failure after the image was unlisted leaves it
	// unlisted, and its blobs for the next start's sweep.
	Remove(ctx context.Context, image ImageDigest) error
}
