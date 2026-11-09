package image

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var digestForm = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// layerTypes are the layer media types the converter takes, in the OCI image
// specification's forms (ADR 0006, ADR 0017).
var layerTypes = map[string]bool{
	"application/vnd.oci.image.layer.v1.tar":                       true,
	"application/vnd.oci.image.layer.v1.tar+gzip":                  true,
	"application/vnd.oci.image.layer.v1.tar+zstd":                  true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar":      true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar+gzip": true,
	"application/vnd.oci.image.layer.nondistributable.v1.tar+zstd": true,
}

// compensationTimeout bounds the removal of a failed pull's staging area, so
// it runs when the pull's own bound has passed.
const compensationTimeout = 30 * time.Second

// compensating is the context of the steps that put right what a pull did
// before it failed: the pull's values, detached from its cancellation.
func compensating(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
}

func (s *service) Pull(ctx context.Context, ref Reference) error {
	// Step 1.
	if ref.Repository == "" {
		return machine.NewError(machine.ErrInvalid, "an image reference needs a repository")
	}
	if !digestForm.MatchString(string(ref.Digest)) {
		return machine.NewError(machine.ErrInvalid, "image reference %q does not name a sha256 digest", ref.Digest)
	}

	s.mu.Lock()
	call, running := s.pulls[ref.Digest]
	if !running {
		call = &pullCall{done: make(chan struct{})}
		s.pulls[ref.Digest] = call
		pullCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.pullTimeout)
		go s.run(pullCtx, cancel, ref, call)
	}
	s.mu.Unlock()
	if running && s.joined != nil {
		s.joined()
	}

	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// run is the one pull of the digest. The entry leaves the map before the
// outcome is announced, so a call arriving after it finds the image listed or
// starts a pull of its own.
func (s *service) run(ctx context.Context, cancel context.CancelFunc, ref Reference, call *pullCall) {
	defer cancel()
	call.err = s.pull(ctx, ref)
	s.mu.Lock()
	delete(s.pulls, ref.Digest)
	s.mu.Unlock()
	close(call.done)
}

// pull runs steps 2 to 10 of ADR 0007. Nothing before the publish changes
// what the cache lists, so each failure leaves nothing visible and removes the
// staging area.
func (s *service) pull(ctx context.Context, ref Reference) error {
	// Step 2.
	if _, err := s.store.Locate(ctx, ref.Digest); err == nil {
		return nil
	} else if !errors.Is(err, machine.ErrImageAbsent) {
		return failed("look for the image in the cache", err)
	}
	staging, err := s.store.Stage(ctx, ref.Digest, s.settings.OutputBytes)
	if err != nil {
		return failed("create the staging area", err)
	}
	if err := s.build(ctx, ref, staging); err != nil {
		ctx, cancel := compensating(ctx)
		defer cancel()
		if removed := s.store.Discard(ctx, staging); removed != nil {
			return machine.NewError(machine.ErrFailed, "%s; the staging area could not be removed: %v", err.Reason, removed)
		}
		return err
	}
	// Step 10. The image is visible: a failure here changes nothing a caller
	// sees, and the sweep removes what is left.
	ctx, cancel := compensating(ctx)
	defer cancel()
	_ = s.store.Discard(ctx, staging)
	return nil
}

// build runs steps 3 to 9.
func (s *service) build(ctx context.Context, ref Reference, staging Staging) *machine.Error {
	// Step 3.
	manifest, err := s.registry.Manifest(ctx, ref, s.settings.Platform)
	if err != nil {
		return failed("fetch the manifest", err)
	}
	// Step 4.
	config, err := s.registry.Config(ctx, ref, manifest.Config)
	if err != nil {
		return failed("fetch the config", err)
	}
	run, err := runSpec(config)
	if err != nil {
		return failed("read the run configuration", err)
	}
	// Step 5.
	layers, err := s.fetchLayers(ctx, ref, staging, manifest.Layers)
	if err != nil {
		return failed("fetch the layers", err)
	}
	// Step 6.
	uuid := filesystemUUID(ref.Digest)
	length, err := s.convert(ctx, ref.Digest, staging, ConversionJob{FilesystemUUID: uuid, Layers: layers, NameSetBytes: s.settings.NameSetBytes})
	if err != nil {
		return failed("convert", err)
	}
	// The converter's report is untrusted: a length outside the output file is
	// refused before Seal is asked to truncate to it.
	if length == 0 || length > s.settings.OutputBytes {
		return failed("convert", fmt.Errorf("the converter reported a length of %d bytes for an output of %d", length, s.settings.OutputBytes))
	}
	// Step 7.
	verity, err := s.store.Seal(ctx, staging, length)
	if err != nil {
		return failed("seal the output", err)
	}
	// Step 8.
	record := Record{Image: ref.Digest, Run: run, Architecture: s.settings.Platform.Architecture, Converter: s.settings.Converter, UUID: uuid, Verity: verity}
	if err := s.store.Record(ctx, staging, record); err != nil {
		return failed("write the image's config and manifest", err)
	}
	// Step 9.
	s.publish.Lock()
	defer s.publish.Unlock()
	if err := s.store.Publish(ctx, staging); err != nil {
		return failed("publish the image", err)
	}
	return nil
}

// fetchLayers is step 5: a layer of a type the converter does not take fails
// the pull before any layer is read, and each layer is stored as the Registry
// verifies it.
func (s *service) fetchLayers(ctx context.Context, ref Reference, staging Staging, layers []Descriptor) ([]StagedLayer, error) {
	for _, l := range layers {
		if !layerTypes[l.MediaType] {
			return nil, fmt.Errorf("layer %s has media type %q, which is not tar, tar+gzip or tar+zstd", l.Digest, l.MediaType)
		}
	}
	staged := make([]StagedLayer, 0, len(layers))
	for i, l := range layers {
		path, err := s.fetchLayer(ctx, ref, staging, i, l)
		if err != nil {
			return nil, fmt.Errorf("layer %d (%s): %w", i, l.Digest, err)
		}
		staged = append(staged, StagedLayer{Path: path, MediaType: l.MediaType})
	}
	return staged, nil
}

func (s *service) fetchLayer(ctx context.Context, ref Reference, staging Staging, index int, layer Descriptor) (string, error) {
	content, err := s.registry.Layer(ctx, ref, layer)
	if err != nil {
		return "", err
	}
	defer content.Close()
	return s.store.PutLayer(ctx, staging, index, content)
}

// runSpec is what the guest runs: the entrypoint and the command as one
// argument vector, and the environment as a map.
func runSpec(c Config) (machine.ExecSpec, error) {
	spec := machine.ExecSpec{WorkingDir: c.WorkingDir}
	spec.Command = append(append(spec.Command, c.Entrypoint...), c.Cmd...)
	if len(c.Env) > 0 {
		spec.Env = make(map[string]string, len(c.Env))
	}
	for _, entry := range c.Env {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || name == "" {
			return machine.ExecSpec{}, fmt.Errorf("environment entry %q is not NAME=value", entry)
		}
		spec.Env[name] = value
	}
	return spec, nil
}

// failed is the outcome of a pull whose step failed.
func failed(step string, err error) *machine.Error {
	return machine.NewError(machine.ErrFailed, "%s: %v", step, err)
}
