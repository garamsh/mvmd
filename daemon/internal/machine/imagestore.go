package machine

import (
	"context"
	"errors"
	"slices"
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
	// Run is what the image's config says to run, its entrypoint and command
	// kept apart.
	Run ImageRun
}

// ImageRun is the run part of an image's config.
type ImageRun struct {
	Entrypoint []string
	Cmd        []string
	Env        map[string]string
	WorkingDir string
	// User is the config's User, in the OCI form; empty when the config sets
	// none.
	User string
	// Joined marks a record written before the three were kept apart: it holds
	// the one argument vector, in Entrypoint, and no Cmd or User. Pulling the
	// image again rewrites the record.
	Joined bool
}

// Exec returns what the guest runs with the image's own configuration: the
// entrypoint and the command as one argument vector.
func (r ImageRun) Exec() ExecSpec {
	return ExecSpec{
		Command:    append(slices.Clone(r.Entrypoint), r.Cmd...),
		Env:        r.Env,
		WorkingDir: r.WorkingDir,
	}
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
