package image_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

const (
	layerTar  = "application/vnd.oci.image.layer.v1.tar"
	layerGzip = "application/vnd.oci.image.layer.v1.tar+gzip"
	outputMax = 1 << 20
	firstUID  = 200000
)

var platform = image.Platform{OS: "linux", Architecture: "amd64"}

// rig is the use case over the in-memory substitutes of every port it drives.
type rig struct {
	registry *memory.Registry
	conv     *memory.Converter
	store    *memory.Store
	uids     *machine.UIDs
	driver   *machinememory.Driver
	sup      *machinememory.Supervision
	settings image.Settings
	svc      image.Service
	joined   chan struct{}
}

func newRig(t *testing.T, options ...image.Option) *rig {
	t.Helper()
	r := &rig{
		registry: memory.NewRegistry(),
		conv:     memory.NewConverter(),
		store:    memory.NewStore(),
		uids:     machine.NewUIDs(machine.UIDRange{First: firstUID, Count: 8}),
		driver:   machinememory.NewDriver(machine.DriverInfo{Name: "memory"}),
		sup:      machinememory.NewSupervision(),
		joined:   make(chan struct{}, 64),
		settings: image.Settings{
			Platform:     platform,
			Converter:    "converter-1",
			Resources:    machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20},
			OutputBytes:  outputMax,
			NameSetBytes: 1 << 16,
			Limit:        time.Minute,
		},
	}
	r.start(options...)
	return r
}

// start builds the service from the rig's current settings.
func (r *rig) start(options ...image.Option) {
	options = append(options, image.WhenJoined(func() { r.joined <- struct{}{} }))
	r.svc = image.NewService(r.registry, r.conv, r.store, r.uids, r.driver, r.sup, r.settings, options...)
}

// awaitJoined waits until n callers have joined a pull already running.
func (r *rig) awaitJoined(t *testing.T, n int) {
	t.Helper()
	for range n {
		select {
		case <-r.joined:
		case <-time.After(5 * time.Second):
			t.Fatal("a caller did not join the pull in flight")
		}
	}
}

// add puts an image in the registry whose layers carry the given tags, so that
// two calls with different tags are two images.
func (r *rig) add(tag string) image.Reference {
	return r.registry.Add("registry.example/app", memory.Image{
		Platform: platform,
		Config: image.Config{
			Entrypoint: []string{"/bin/app"},
			Cmd:        []string{"--serve", tag},
			Env:        []string{"A=1", "B=x=y"},
			WorkingDir: "/srv",
		},
		Layers: []memory.Layer{
			{MediaType: layerTar, Content: []byte("base layer of " + tag)},
			{MediaType: layerGzip, Content: []byte("top layer of " + tag)},
		},
	})
}

func (r *rig) pull(ref image.Reference) error {
	return r.svc.Pull(context.Background(), ref)
}

// calls is how many times the step of a port was called. The steps are named
// in the order a pull reaches them.
func (r *rig) calls(step string) int {
	switch step {
	case "locate", "stage", "lend", "reclaim", "put-layer", "seal", "record", "publish", "publish-sync", "discard", "sweep":
		return r.store.Calls(step)
	case "manifest", "config", "layer":
		return r.registry.Calls(step)
	case "describe", "boot":
		return r.driver.Calls(step)
	case "start", "terminate":
		return r.sup.Calls(step)
	case "convert":
		return r.conv.Calls(step)
	}
	panic("no step " + step)
}

// failAt makes the step fail with err and heal makes it succeed again.
func (r *rig) failAt(step string, err error) { r.failures(step).FailAt(step, err) }
func (r *rig) heal(step string)              { r.failures(step).Heal(step) }

func (r *rig) failures(step string) *machinememory.Failures {
	switch step {
	case "locate", "stage", "lend", "reclaim", "put-layer", "seal", "record", "publish", "publish-sync", "discard", "sweep":
		return &r.store.Failures
	case "manifest", "config", "layer":
		return &r.registry.Failures
	case "describe", "boot":
		return &r.driver.Failures
	case "start", "terminate":
		return &r.sup.Failures
	case "convert":
		return &r.conv.Failures
	}
	panic("no step " + step)
}

// conversionID is the identifier the converter VM of the image has.
func conversionID(ref image.Reference) machine.MachineID {
	return machine.MachineID("convert-" + strings.TrimPrefix(string(ref.Digest), "sha256:"))
}

// requireListed asserts the image is in the cache and Locate finds it.
func (r *rig) requireListed(t *testing.T, ref image.Reference) {
	t.Helper()
	assert.Contains(t, r.store.Listed(), ref.Digest)
	_, err := r.store.Locate(context.Background(), ref.Digest)
	require.NoError(t, err)
}

// requireNothingVisible asserts the cache lists nothing and holds no staging
// area, and that no converter VM or user of one is left.
func (r *rig) requireNothingVisible(t *testing.T, ref image.Reference) {
	t.Helper()
	assert.Empty(t, r.store.Listed(), "the cache lists an image")
	_, err := r.store.Locate(context.Background(), ref.Digest)
	assert.ErrorIs(t, err, machine.ErrImageAbsent)
	assert.Zero(t, r.store.Staging(), "a staging area is left")
	_, running := r.sup.Running(conversionID(ref))
	assert.False(t, running, "the converter's VMM is left")
	assert.Zero(t, r.uids.Held(), "a converter user is left held")
}

func requireKind(t *testing.T, err, kind error) *machine.Error {
	t.Helper()
	require.ErrorIs(t, err, kind)
	var e *machine.Error
	require.ErrorAs(t, err, &e)
	return e
}
