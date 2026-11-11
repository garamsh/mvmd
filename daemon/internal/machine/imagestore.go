package machine

import (
	"context"
	"errors"
	"time"
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

// ImageInfo is what the node holds of an image, for a client to decide whether
// to keep it.
type ImageInfo struct {
	Digest ImageDigest
	// SizeBytes is the disk space the node's files for the image occupy:
	// allocated blocks, summed over every file the node holds for it.
	SizeBytes uint64
	// CreatedAt is when the node stored the image, and the zero time for an
	// image stored before the node recorded it.
	CreatedAt time.Time
}

// ImageStore is the conversation with the node's image cache.
type ImageStore interface {
	// Locate returns what the node holds of the image, or ErrImageAbsent.
	Locate(ctx context.Context, image ImageDigest) (LocatedImage, error)
	// List returns every image the node holds, ordered by digest.
	List(ctx context.Context) ([]ImageInfo, error)
	// Remove unlists the image and reclaims the blobs only it reached. An
	// absent image succeeds. A failure after the image was unlisted leaves it
	// unlisted, and its blobs for the next start's sweep.
	Remove(ctx context.Context, image ImageDigest) error
}
