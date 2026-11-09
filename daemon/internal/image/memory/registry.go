// Package memory holds the in-memory substitutes for the ports of package
// image. They serve tests and development; each can be told to fail at a named
// step, so the state-after-failure column of ADR 0007 can be tested.
package memory

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// Layer is a layer a Registry serves: its media type and bytes.
type Layer struct {
	MediaType string
	Content   []byte
}

// Image is an image a Registry holds for one platform.
type Image struct {
	Platform image.Platform
	Config   image.Config
	Layers   []Layer
}

// Registry is an in-memory image.Registry. Its steps are "manifest", "config"
// and "layer". It checks every layer against its descriptor's size and digest
// as the contract says, so a layer changed after it was added fails when read.
type Registry struct {
	machinememory.Failures

	mu     sync.Mutex
	images map[string]stored
	blobs  map[string][]byte
}

type stored struct {
	platform image.Platform
	manifest image.Manifest
	config   image.Config
}

// NewRegistry returns a Registry that holds no image.
func NewRegistry() *Registry {
	return &Registry{images: make(map[string]stored), blobs: make(map[string][]byte)}
}

// Add makes the repository hold the image and returns the reference that
// names it. The same image added again gets the same reference.
func (r *Registry) Add(repository string, img Image) image.Reference {
	r.mu.Lock()
	defer r.mu.Unlock()
	config := img.Config
	encoded := fmt.Appendf(nil, "%#v", config)
	manifest := image.Manifest{Config: image.Descriptor{MediaType: "application/vnd.oci.image.config.v1+json", Digest: digestOf(encoded), Size: int64(len(encoded))}}
	id := sha256.New()
	fmt.Fprintf(id, "%v|%s|", img.Platform, manifest.Config.Digest)
	for _, l := range img.Layers {
		d := image.Descriptor{MediaType: l.MediaType, Digest: digestOf(l.Content), Size: int64(len(l.Content))}
		manifest.Layers = append(manifest.Layers, d)
		r.blobs[d.Digest] = slices.Clone(l.Content)
		fmt.Fprintf(id, "%s,", d.Digest)
	}
	digest := "sha256:" + hex.EncodeToString(id.Sum(nil))
	r.images[repository+"@"+digest] = stored{platform: img.Platform, manifest: manifest, config: config}
	return image.Reference{Repository: repository, Digest: machine.ImageDigest(digest)}
}

// Tamper replaces the bytes served for the layer's digest, as a registry
// serving other content than it advertised does.
func (r *Registry) Tamper(layer image.Descriptor, content []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.blobs[layer.Digest] = slices.Clone(content)
}

// Manifest implements image.Registry.
func (r *Registry) Manifest(_ context.Context, ref image.Reference, platform image.Platform) (image.Manifest, error) {
	if err := r.Enter("manifest"); err != nil {
		return image.Manifest{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.images[ref.Repository+"@"+string(ref.Digest)]
	if !ok {
		return image.Manifest{}, fmt.Errorf("%s@%s is not in the registry", ref.Repository, ref.Digest)
	}
	if s.platform != platform {
		return image.Manifest{}, fmt.Errorf("no manifest for platform %v", platform)
	}
	m := s.manifest
	m.Layers = slices.Clone(m.Layers)
	return m, nil
}

// Config implements image.Registry.
func (r *Registry) Config(_ context.Context, ref image.Reference, config image.Descriptor) (image.Config, error) {
	if err := r.Enter("config"); err != nil {
		return image.Config{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.images[ref.Repository+"@"+string(ref.Digest)]
	if !ok || s.manifest.Config != config {
		return image.Config{}, fmt.Errorf("config %s is not in the registry", config.Digest)
	}
	c := s.config
	c.Entrypoint, c.Cmd, c.Env = slices.Clone(c.Entrypoint), slices.Clone(c.Cmd), slices.Clone(c.Env)
	return c, nil
}

// Layer implements image.Registry.
func (r *Registry) Layer(_ context.Context, ref image.Reference, layer image.Descriptor) (io.ReadCloser, error) {
	if err := r.Enter("layer"); err != nil {
		return nil, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	content, ok := r.blobs[layer.Digest]
	if !ok {
		return nil, fmt.Errorf("layer %s is not in the registry", layer.Digest)
	}
	return io.NopCloser(&verifying{src: bytes.NewReader(slices.Clone(content)), want: layer, sum: sha256.New()}), nil
}

// verifying reads the layer and fails at its end, or sooner, when what it read
// is not what the descriptor says.
type verifying struct {
	src  io.Reader
	want image.Descriptor
	sum  interface {
		io.Writer
		Sum([]byte) []byte
	}
	read int64
}

func (v *verifying) Read(p []byte) (int, error) {
	n, err := v.src.Read(p)
	v.sum.Write(p[:n])
	v.read += int64(n)
	if v.read > v.want.Size {
		return n, fmt.Errorf("layer %s is longer than its size %d", v.want.Digest, v.want.Size)
	}
	if errors.Is(err, io.EOF) {
		switch {
		case v.read != v.want.Size:
			return n, fmt.Errorf("layer %s is %d bytes, not %d", v.want.Digest, v.read, v.want.Size)
		case "sha256:"+hex.EncodeToString(v.sum.Sum(nil)) != v.want.Digest:
			return n, fmt.Errorf("layer %s does not hash to its digest", v.want.Digest)
		}
	}
	return n, err
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
