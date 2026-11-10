package app_test

import (
	"context"
	"errors"
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/image"
	imagememory "github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const failed = v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED

// addImage puts an image in the memory Registry for the platform the node
// selects, and returns the request that pulls it.
func addImage(a *app.App, tag string) *v1alpha2.PullImageRequest {
	ref := a.Substitutes.Registry.Add("registry.example/app", imagememory.Image{
		Platform: image.Platform{OS: "linux", Architecture: runtime.GOARCH},
		Config:   image.Config{Entrypoint: []string{"/bin/app"}, Cmd: []string{tag}},
		Layers: []imagememory.Layer{
			{MediaType: "application/vnd.oci.image.layer.v1.tar", Content: []byte("layer of " + tag)},
		},
	})
	return &v1alpha2.PullImageRequest{Image: &v1alpha2.ImageRef{Repository: ref.Repository, Digest: string(ref.Digest)}}
}

// PullImage reaches the use case, which publishes the image to the cache.
func TestPull_ThroughTheAPIPublishesTheImage(t *testing.T) {
	ctx := context.Background()
	var req *v1alpha2.PullImageRequest
	n := startWith(t, testConfig(t), func(a *app.App) { req = addImage(a, "one") })
	cache := n.app.Substitutes.Cache
	require.Empty(t, cache.Listed(), "the setup: nothing is listed before the pull")

	_, err := n.client.PullImage(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, []machine.ImageDigest{machine.ImageDigest(req.GetImage().GetDigest())}, cache.Listed())

	_, err = n.client.PullImage(ctx, req)
	assert.NoError(t, err, "a digest already present is success")
	assert.Len(t, cache.Listed(), 1)

	// The control: an image the registry does not hold fails the call and
	// lists nothing more.
	_, err = n.client.PullImage(ctx, &v1alpha2.PullImageRequest{Image: &v1alpha2.ImageRef{Repository: "registry.example/app", Digest: digest}})
	requireKind(t, err, failed)
	assert.Len(t, cache.Listed(), 1)
}

// The use case's failures end the call as FAILED with the step that failed,
// whether the registry or the converter broke.
func TestPull_AFailedStepEndsTheCallWithItsReason(t *testing.T) {
	tests := []struct {
		name, step, reason string
		fail               func(a *app.App, err error)
	}{
		{"registry", "manifest", "fetch the manifest", func(a *app.App, err error) { a.Substitutes.Registry.FailAt("manifest", err) }},
		{"converter", "convert", "convert", func(a *app.App, err error) { a.Substitutes.Converter.FailAt("convert", err) }},
		{"image cache", "publish", "publish the image", func(a *app.App, err error) { a.Substitutes.Cache.FailAt("publish", err) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req *v1alpha2.PullImageRequest
			n := startWith(t, testConfig(t), func(a *app.App) {
				req = addImage(a, tt.name)
				tt.fail(a, errors.New("injected "+tt.name))
			})

			_, err := n.client.PullImage(context.Background(), req)
			requireKind(t, err, failed)
			assert.ErrorContains(t, err, tt.reason)
			assert.ErrorContains(t, err, "injected "+tt.name)
			assert.Empty(t, n.app.Substitutes.Cache.Listed())

			// The control: the same pull succeeds once the step is healed.
			switch tt.name {
			case "registry":
				n.app.Substitutes.Registry.Heal("manifest")
			case "converter":
				n.app.Substitutes.Converter.Heal("convert")
			default:
				n.app.Substitutes.Cache.Heal("publish")
			}
			_, err = n.client.PullImage(context.Background(), req)
			assert.NoError(t, err)
			assert.Len(t, n.app.Substitutes.Cache.Listed(), 1)
		})
	}
}

// A reference the validator refuses never reaches the registry.
func TestPull_AnInvalidReferenceIsRejectedBeforeTheUseCase(t *testing.T) {
	ctx := context.Background()
	var good *v1alpha2.PullImageRequest
	n := startWith(t, testConfig(t), func(a *app.App) { good = addImage(a, "one") })
	raw := n.raw(t)
	registry := n.app.Substitutes.Registry

	for name, ref := range map[string]*v1alpha2.ImageRef{
		"a tag":           {Repository: "registry.example/app:latest", Digest: good.GetImage().GetDigest()},
		"a digest in it":  {Repository: "registry.example/app@" + digest, Digest: good.GetImage().GetDigest()},
		"a short digest":  {Repository: "registry.example/app", Digest: "sha256:abc"},
		"no repository":   {Digest: good.GetImage().GetDigest()},
		"no image at all": nil,
		"an md5 digest":   {Repository: "registry.example/app", Digest: "md5:0123456789abcdef0123456789abcdef"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := raw.PullImage(ctx, &v1alpha2.PullImageRequest{Image: ref})
			requireKind(t, err, rejected)
		})
	}
	assert.Zero(t, registry.Calls("manifest"), "no refused call reached the registry")

	// The control: the valid reference is served, and reaches it.
	_, err := raw.PullImage(ctx, good)
	require.NoError(t, err)
	assert.NotZero(t, registry.Calls("manifest"))
}

// The pull bound is the configured one: a conversion that never ends is cut
// off at it, and a node with a longer bound waits for a conversion that takes
// less.
func TestPull_TheConfiguredBoundEndsAPull(t *testing.T) {
	ctx := context.Background()
	slow := func(ctx context.Context, _ machine.MachineID, _ image.ConversionJob) (uint64, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	}

	cfg := testConfig(t)
	cfg.PullTimeout = "100ms"
	var req *v1alpha2.PullImageRequest
	n := startWith(t, cfg, func(a *app.App) {
		req = addImage(a, "slow")
		a.Substitutes.Converter.Serve(slow)
	})
	began := time.Now()
	_, err := n.client.PullImage(ctx, req)
	assert.Less(t, time.Since(began), 20*time.Second, "the pull ended at its bound, not at the conversion limit")
	requireKind(t, err, failed)
	assert.ErrorContains(t, err, "deadline exceeded")
	assert.Empty(t, n.app.Substitutes.Cache.Listed())

	// The control: with a bound that is not reached, the same converter
	// shape (here one that answers) completes.
	cfg = testConfig(t)
	cfg.PullTimeout = "1m"
	n = startWith(t, cfg, func(a *app.App) { req = addImage(a, "quick") })
	_, err = n.client.PullImage(ctx, req)
	assert.NoError(t, err)
}

// The Image Store is swept once at start, before the socket exists: a staging
// area a crash left is gone by the time a client can connect.
func TestPull_TheStoreIsSweptBeforeServing(t *testing.T) {
	var cache *imagememory.Store
	n := startWith(t, testConfig(t), func(a *app.App) {
		cache = a.Substitutes.Cache
		_, err := cache.Stage(context.Background(), digest, 1)
		require.NoError(t, err)
		require.Equal(t, 1, cache.Staging(), "the setup: a crash left a staging area")
	})

	assert.Zero(t, cache.Staging(), "the sweep removed it before the daemon was ready")
	assert.Equal(t, 1, cache.Calls("sweep"), "once")
	_ = n
}

// A store that cannot be swept is a daemon that does not serve.
func TestPull_AFailedSweepStopsTheStart(t *testing.T) {
	cfg := testConfig(t)
	a, err := app.New(cfg)
	require.NoError(t, err)
	a.Substitutes.Cache.FailAt("sweep", errors.New("disk gone"))

	serveCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	err = a.Serve(serveCtx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "sweep the Image Store")
	assert.ErrorContains(t, err, "disk gone")
	assert.NoFileExists(t, cfg.Socket.Path, "nothing was served")

	// The control: the same node without the fault serves.
	n := start(t, testConfig(t))
	assert.NotNil(t, n.client)
}

// The converter VMs a previous mvmd left are ended at start, so the users they
// run as are not handed out again, and before anything is served.
func TestPull_ConversionsOfAPreviousMvmdAreEndedAtStart(t *testing.T) {
	ctx := context.Background()
	user := machine.User{UID: 1000, GID: 1000}
	var sup interface {
		Live(context.Context) ([]machine.MachineID, error)
	}
	startWithConversion := func(a *app.App) {
		s := a.Substitutes.ConversionSupervision
		sup = s
		require.NoError(t, s.Start(ctx, "convert-0123", machine.ProcessDescription{}, user))
		live, err := s.Live(ctx)
		require.NoError(t, err)
		require.Equal(t, []machine.MachineID{"convert-0123"}, live, "the setup: a conversion outlived mvmd")
	}
	startWith(t, testConfig(t), startWithConversion)

	live, err := sup.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live, "the conversion's VMM was ended")

	// The user is free for a Machine: the first of the range, which the
	// conversion held, is the one the next Machine gets.
	n := startWith(t, testConfig(t), func(a *app.App) { a.Substitutes.Images.Provide(digest) })
	_, err = n.client.CreateMachine(ctx, create("m"))
	renew(t, n, 1)
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	user2, ok := n.app.Substitutes.Supervisors.User("m")
	require.True(t, ok)
	assert.Equal(t, uint32(1000), user2.UID, "control: a Machine takes the first user when no conversion holds it")
}

// A conversion that cannot be ended stops the start: its user would otherwise
// be handed to a Machine while the VMM runs as it.
func TestPull_AConversionThatCannotBeEndedStopsTheStart(t *testing.T) {
	cfg := testConfig(t)
	a, err := app.New(cfg)
	require.NoError(t, err)
	ctx := context.Background()
	require.NoError(t, a.Substitutes.ConversionSupervision.Start(ctx, "convert-0123", machine.ProcessDescription{}, machine.User{UID: 1000, GID: 1000}))
	a.Substitutes.ConversionSupervision.FailAt("terminate", errors.New("cgroup busy"))

	serveCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	err = a.Serve(serveCtx)
	require.Error(t, err)
	assert.ErrorContains(t, err, "convert-0123")
	assert.ErrorContains(t, err, "cgroup busy")
	_, statErr := os.Stat(cfg.Socket.Path)
	assert.True(t, os.IsNotExist(statErr), "nothing was served")
	assert.Equal(t, 0, a.Substitutes.Cache.Calls("sweep"), "the sweep did not run over a conversion still writing")
}

// The Machines' supervisors and the conversions' are separate: a conversion is
// not one of the lifecycle's Machines, and so is not reconciled as one.
func TestPull_ConversionsAreNotTheLifecyclesMachines(t *testing.T) {
	ctx := context.Background()
	n := startWith(t, testConfig(t), nil)
	sub := n.app.Substitutes

	require.NoError(t, sub.ConversionSupervision.Start(ctx, "convert-0123", machine.ProcessDescription{}, machine.User{UID: 1000, GID: 1000}))
	live, err := sub.Supervisors.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live, "a conversion is not among the supervisors the deadline renews")
	_, err = n.client.ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	assert.NoError(t, err)
}
