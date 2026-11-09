package image

import (
	"context"
	"io"
)

// Registry is the conversation with the registry an image is pulled from.
// Every blob is fetched by digest and checked against its size and SHA-256;
// the host parses only the JSON of manifests, indexes and configs (ADR 0005).
type Registry interface {
	// Manifest fetches and checks the manifest the reference's digest names and
	// returns the one for the platform: the manifest itself, or the one an
	// index selects, fetched and checked the same way. An index with none for
	// the platform is an error.
	Manifest(ctx context.Context, ref Reference, platform Platform) (Manifest, error)
	// Config fetches and checks the config blob and decodes it.
	Config(ctx context.Context, ref Reference, config Descriptor) (Config, error)
	// Layer streams the layer's bytes as the registry serves them. The
	// reader fails when the bytes read differ from the descriptor's size or
	// digest, no later than at its end, so a caller that reads to the end has
	// verified them. The caller closes it.
	Layer(ctx context.Context, ref Reference, layer Descriptor) (io.ReadCloser, error)
}
