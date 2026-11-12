// Package ociregistry is the Registry port of package image over an OCI
// distribution registry, on oras-go's client. It fetches by digest only and
// checks every byte it hands on against the descriptor that named it (ADR
// 0005, ADR 0007).
package ociregistry

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"

	"github.com/garamsh/mvmd/daemon/internal/image"
)

// maxJSONBytes bounds a manifest, an index and a config, the only blobs the
// host parses.
const maxJSONBytes = 4 << 20

// Credential is what a registry is asked to accept as the node's identity. It
// prints as redacted, so a log line that holds one leaks nothing.
type Credential struct {
	Username string
	Password string
}

// String implements fmt.Stringer.
func (Credential) String() string { return "[redacted]" }

// LogValue implements slog.LogValuer.
func (Credential) LogValue() slog.Value { return slog.StringValue("[redacted]") }

// Options is what a Registry is built from.
type Options struct {
	// Credentials returns the credential for a registry, named as host:port,
	// and a Credential of no username for none; nil pulls everything
	// anonymously. It is the only way a credential reaches the adapter, which
	// reads no file and no environment variable.
	Credentials func(ctx context.Context, registry string) (Credential, error)
	// RootCAs returns the roots a registry's certificate is verified against,
	// and is called at each request, so a change takes effect without a new
	// Registry. When set, it replaces HTTPClient, which is not used; nil
	// verifies against the system roots.
	RootCAs func(ctx context.Context) (*x509.CertPool, error)
	// HTTPClient carries every request; nil is a client with the default
	// transport. Registries are reached over HTTPS only.
	HTTPClient *http.Client
}

// Registry is an image.Registry over OCI distribution registries.
type Registry struct {
	client *auth.Client
}

// New returns a Registry.
func New(opts Options) *Registry {
	httpClient := opts.HTTPClient
	if opts.RootCAs != nil {
		httpClient = &http.Client{Transport: &rootsTransport{roots: opts.RootCAs}}
	}
	client := &auth.Client{
		Client: httpClient,
		Header: http.Header{"User-Agent": {"mvmd"}},
		Cache:  auth.NewCache(),
	}
	if opts.Credentials != nil {
		client.Credential = func(ctx context.Context, registry string) (auth.Credential, error) {
			c, err := opts.Credentials(ctx, registry)
			if err != nil {
				return auth.EmptyCredential, err
			}
			return auth.Credential{Username: c.Username, Password: c.Password}, nil
		}
	}
	return &Registry{client: client}
}

// Manifest implements image.Registry.
func (r *Registry) Manifest(ctx context.Context, ref image.Reference, platform image.Platform) (image.Manifest, error) {
	repo, err := r.repository(ref)
	if err != nil {
		return image.Manifest{}, err
	}
	desc, body, err := repo.Manifests().FetchReference(ctx, string(ref.Digest))
	if err != nil {
		return image.Manifest{}, fail(ctx, "fetch the manifest", err)
	}
	top, err := readVerified(ctx, body, desc, "manifest")
	if err != nil {
		return image.Manifest{}, err
	}
	switch desc.MediaType {
	case ocispec.MediaTypeImageManifest:
		return parseManifest(top)
	case ocispec.MediaTypeImageIndex:
		chosen, err := selectPlatform(top, platform)
		if err != nil {
			return image.Manifest{}, err
		}
		body, err := repo.Manifests().Fetch(ctx, chosen)
		if err != nil {
			return image.Manifest{}, fail(ctx, "fetch the platform manifest", err)
		}
		content, err := readVerified(ctx, body, chosen, "platform manifest")
		if err != nil {
			return image.Manifest{}, err
		}
		return parseManifest(content)
	default:
		return image.Manifest{}, fmt.Errorf("manifest %s has media type %q, which is not an OCI image manifest or index", ref.Digest, desc.MediaType)
	}
}

// Config implements image.Registry.
func (r *Registry) Config(ctx context.Context, ref image.Reference, config image.Descriptor) (image.Config, error) {
	repo, err := r.repository(ref)
	if err != nil {
		return image.Config{}, err
	}
	desc, err := toOCI(config)
	if err != nil {
		return image.Config{}, err
	}
	body, err := repo.Blobs().Fetch(ctx, desc)
	if err != nil {
		return image.Config{}, fail(ctx, "fetch the config", err)
	}
	content, err := readVerified(ctx, body, desc, "config")
	if err != nil {
		return image.Config{}, err
	}
	var decoded struct {
		Config struct {
			Entrypoint []string
			Cmd        []string
			Env        []string
			WorkingDir string
		}
	}
	if err := json.Unmarshal(content, &decoded); err != nil {
		return image.Config{}, fmt.Errorf("decode the config %s: %v", config.Digest, err)
	}
	c := decoded.Config
	return image.Config{Entrypoint: c.Entrypoint, Cmd: c.Cmd, Env: c.Env, WorkingDir: c.WorkingDir}, nil
}

// Layer implements image.Registry.
func (r *Registry) Layer(ctx context.Context, ref image.Reference, layer image.Descriptor) (io.ReadCloser, error) {
	repo, err := r.repository(ref)
	if err != nil {
		return nil, err
	}
	desc, err := toOCI(layer)
	if err != nil {
		return nil, err
	}
	body, err := repo.Blobs().Fetch(ctx, desc)
	if err != nil {
		return nil, fail(ctx, "fetch the layer", err)
	}
	return newVerifying(body, layer), nil
}

// repository is the client of the reference's repository, refusing a
// reference that names a digest of anything but SHA-256, or a tag.
func (r *Registry) repository(ref image.Reference) (*remote.Repository, error) {
	if _, err := toOCI(image.Descriptor{Digest: string(ref.Digest)}); err != nil {
		return nil, fmt.Errorf("image reference %q: %w", ref.Digest, err)
	}
	repo, err := remote.NewRepository(ref.Repository)
	if err != nil {
		return nil, fmt.Errorf("repository %q: %v", ref.Repository, err)
	}
	if repo.Reference.Reference != "" {
		return nil, fmt.Errorf("repository %q names a tag or a digest; a pull names the digest in the reference", ref.Repository)
	}
	repo.Client = r.client
	repo.ManifestMediaTypes = []string{ocispec.MediaTypeImageManifest, ocispec.MediaTypeImageIndex}
	return repo, nil
}

// readVerified reads a manifest, an index or a config of at most maxJSONBytes
// and returns it only if it is the descriptor's.
func readVerified(ctx context.Context, body io.ReadCloser, desc ocispec.Descriptor, what string) ([]byte, error) {
	d, err := describe(desc)
	if err != nil {
		body.Close()
		return nil, err
	}
	content, err := readAll(body, d, maxJSONBytes)
	if err != nil {
		return nil, fail(ctx, "read the "+what, err)
	}
	return content, nil
}

func parseManifest(content []byte) (image.Manifest, error) {
	var m ocispec.Manifest
	if err := json.Unmarshal(content, &m); err != nil {
		return image.Manifest{}, fmt.Errorf("decode the manifest: %v", err)
	}
	config, err := describe(m.Config)
	if err != nil {
		return image.Manifest{}, fmt.Errorf("manifest config: %w", err)
	}
	out := image.Manifest{Config: config}
	for i, l := range m.Layers {
		d, err := describe(l)
		if err != nil {
			return image.Manifest{}, fmt.Errorf("manifest layer %d: %w", i, err)
		}
		out.Layers = append(out.Layers, d)
	}
	return out, nil
}

// selectPlatform returns the first manifest of the index for the platform.
func selectPlatform(content []byte, platform image.Platform) (ocispec.Descriptor, error) {
	var idx ocispec.Index
	if err := json.Unmarshal(content, &idx); err != nil {
		return ocispec.Descriptor{}, fmt.Errorf("decode the index: %v", err)
	}
	for _, m := range idx.Manifests {
		if m.Platform == nil || m.Platform.OS != platform.OS || m.Platform.Architecture != platform.Architecture {
			continue
		}
		if m.MediaType != ocispec.MediaTypeImageManifest {
			return ocispec.Descriptor{}, fmt.Errorf("the index's manifest for %s/%s has media type %q, which is not an OCI image manifest", platform.OS, platform.Architecture, m.MediaType)
		}
		if _, err := describe(m); err != nil {
			return ocispec.Descriptor{}, fmt.Errorf("the index's manifest for %s/%s: %w", platform.OS, platform.Architecture, err)
		}
		return m, nil
	}
	return ocispec.Descriptor{}, fmt.Errorf("the index has no manifest for %s/%s", platform.OS, platform.Architecture)
}

// describe is the boundary for a descriptor read from the registry: its digest
// is SHA-256 and its size is not negative.
func describe(d ocispec.Descriptor) (image.Descriptor, error) {
	if err := d.Digest.Validate(); err != nil || d.Digest.Algorithm() != digest.SHA256 {
		return image.Descriptor{}, fmt.Errorf("digest %q is not a SHA-256 digest", d.Digest)
	}
	if d.Size < 0 {
		return image.Descriptor{}, fmt.Errorf("blob %s declares size %d", d.Digest, d.Size)
	}
	return image.Descriptor{MediaType: d.MediaType, Digest: string(d.Digest), Size: d.Size}, nil
}

// toOCI is describe's inverse, for a descriptor the application hands in.
func toOCI(d image.Descriptor) (ocispec.Descriptor, error) {
	o := ocispec.Descriptor{MediaType: d.MediaType, Digest: digest.Digest(d.Digest), Size: d.Size}
	if _, err := describe(o); err != nil {
		return ocispec.Descriptor{}, err
	}
	return o, nil
}

// fail returns the library's failure without its type, keeping the context's
// error when the context ended the call.
func fail(ctx context.Context, op string, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return fmt.Errorf("%s: %w", op, cerr)
	}
	if errors.Is(err, errSize) || errors.Is(err, errDigest) {
		return fmt.Errorf("%s: %w", op, err)
	}
	return fmt.Errorf("%s: %s", op, strings.TrimSpace(err.Error()))
}
