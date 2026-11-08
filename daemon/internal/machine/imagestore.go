package machine

import (
	"context"
	"errors"
)

// ErrImageAbsent is what ImageStore.Locate returns for an image the node does
// not hold.
var ErrImageAbsent = errors.New("image not on the node")

// ImageStore is the conversation with the node's image cache.
type ImageStore interface {
	// Locate returns the path of the image's read-only filesystem file, or
	// ErrImageAbsent.
	Locate(ctx context.Context, image ImageDigest) (path string, err error)
}
