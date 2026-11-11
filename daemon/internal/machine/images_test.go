package machine_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// spare is an image on the node that no Machine or snapshot uses: the near
// miss of every refusal below.
const spare machine.ImageDigest = "sha256:spare"

func (e *env) withSpare(t *testing.T) {
	t.Helper()
	e.images.Provide(spare)
	e.requireImages(t, image, spare)
}

func (e *env) requireImages(t *testing.T, want ...machine.ImageDigest) {
	t.Helper()
	got, err := e.svc.ListImages(context.Background())
	require.NoError(t, err)
	digests := make([]machine.ImageDigest, 0, len(got))
	for _, info := range got {
		digests = append(digests, info.Digest)
	}
	require.Equal(t, append([]machine.ImageDigest{}, want...), digests)
}

// Row "List images", column On success.
func TestListImages_ReportsWhatTheNodeHoldsOrderedByDigest(t *testing.T) {
	e := newEnv()
	e.images.Provide("sha256:zzz")
	e.images.Provide("sha256:aaa")

	got, err := e.svc.ListImages(context.Background())

	require.NoError(t, err)
	assert.Equal(t, []machine.ImageInfo{{Digest: "sha256:aaa"}, {Digest: image}, {Digest: "sha256:zzz"}}, got)
}

// Row "List images", column After failure.
func TestListImages_StoreFailure(t *testing.T) {
	e := newEnv()
	e.images.FailAt("list", errBoom)

	_, err := e.svc.ListImages(context.Background())

	requireKind(t, err, machine.ErrFailed)
	e.images.Heal("list")
	e.requireImages(t, image)
}

// Row "Remove image", columns On success and Repeat.
func TestRemoveImage_RemovesTheImageAndRepeatSucceeds(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.withSpare(t)

	require.NoError(t, e.svc.RemoveImage(ctx, spare))
	e.requireImages(t, image)

	require.NoError(t, e.svc.RemoveImage(ctx, spare), "an image that is already gone is removed")
	require.NoError(t, e.svc.RemoveImage(ctx, "sha256:never-held"), "so is one the node never held")
	e.requireImages(t, image)

	requireKind(t, e.svc.RemoveImage(ctx, ""), machine.ErrInvalid)
}

// Row "Remove image", column Needs: no Machine references the image.
func TestRemoveImage_RefusesAnImageAMachineReferencesInAnyState(t *testing.T) {
	ctx := context.Background()
	for _, state := range []machine.State{machine.StateCreated, machine.StateRunning, machine.StatePaused, machine.StateStopped, machine.StateFaulted} {
		t.Run(state.String(), func(t *testing.T) {
			e := newEnv()
			e.withSpare(t)
			e.inState(t, "m", state)

			require.NoError(t, e.svc.RemoveImage(ctx, spare), "the near miss: an unused image goes")
			err := e.svc.RemoveImage(ctx, image)

			requireKind(t, err, machine.ErrPreconditionFailed)
			e.requireImages(t, image)
			e.requireState(t, "m", state)

			e.deleted(t, "m")
			require.NoError(t, e.svc.RemoveImage(ctx, image), "once the Machine is deleted nothing uses the image")
			e.requireImages(t)
		})
	}
}

// Row "Remove image", column Needs: no snapshot references the image, found in
// the Snapshot Store, so a snapshot whose Machine is gone still holds it.
func TestRemoveImage_RefusesAnImageASnapshotReferences(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.images.Provide(spare)
	e.snapshotted(t, "src", machine.StateRunning)
	e.deleted(t, "src")
	e.requireNoMachine(t, "src")

	require.NoError(t, e.svc.RemoveImage(ctx, spare), "the near miss: an unused image goes")
	err := e.svc.RemoveImage(ctx, image)

	requireKind(t, err, machine.ErrPreconditionFailed)
	e.requireImages(t, image)
	assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())

	require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"))
	require.NoError(t, e.svc.RemoveImage(ctx, image), "once the snapshot is deleted nothing uses the image")
	e.requireImages(t)
}

// Row "Remove image", column Needs: no Create or restore is locating it.
func TestRemoveImage_RefusesAnImageACallIsLocating(t *testing.T) {
	ctx := context.Background()

	t.Run("a Create", func(t *testing.T) {
		e := newEnv()
		e.withSpare(t)
		entered, release := e.images.Hold("locate")
		creating := make(chan error, 1)
		go func() { creating <- e.svc.Create(ctx, "m", request("m"), baseSpec()) }()
		<-entered

		require.NoError(t, e.svc.RemoveImage(ctx, spare), "the near miss: another image goes")
		err := e.svc.RemoveImage(ctx, image)
		release()

		requireKind(t, err, machine.ErrPreconditionFailed)
		require.NoError(t, <-creating)
		e.requireState(t, "m", machine.StateCreated)
		e.requireImages(t, image)
	})

	// The snapshot restored from also references the image, so this case is
	// refused by the snapshot scan as well; it pins the outcome, and the lock
	// is what the Create case above pins.
	t.Run("a restore", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.images.Provide(spare)
		e.snapshotted(t, "src", machine.StateRunning)
		e.deleted(t, "src")
		entered, release := e.images.Hold("locate")
		restoring := make(chan error, 1)
		go func() { restoring <- e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) }()
		<-entered

		require.NoError(t, e.svc.RemoveImage(ctx, spare), "the near miss: another image goes")
		err := e.svc.RemoveImage(ctx, image)
		release()

		requireKind(t, err, machine.ErrPreconditionFailed)
		require.NoError(t, <-restoring)
		e.requireImages(t, image)
	})
}

// Row "Remove image", column After failure.
func TestRemoveImage_StoreFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.withSpare(t)
	e.images.FailAt("remove", errBoom)

	requireKind(t, e.svc.RemoveImage(ctx, spare), machine.ErrFailed)

	e.images.Heal("remove")
	require.NoError(t, e.svc.RemoveImage(ctx, spare), "a repeat after the failure removes it")
	e.requireImages(t, image)
}

func TestRemoveImage_AFailureToListSnapshotsRemovesNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.withSpare(t)
	e.snaps.FailAt("list", errBoom)

	requireKind(t, e.svc.RemoveImage(ctx, spare), machine.ErrFailed)
	e.requireImages(t, image, spare)

	e.snaps.Heal("list")
	require.NoError(t, e.svc.RemoveImage(ctx, spare))
}

// deleted stops the Machine unless it is already down, and deletes it.
func (e *env) deleted(t *testing.T, id machine.MachineID) {
	t.Helper()
	ctx := context.Background()
	m, err := e.svc.Inspect(ctx, id)
	require.NoError(t, err)
	if m.State == machine.StateRunning || m.State == machine.StatePaused {
		require.NoError(t, e.svc.Stop(ctx, id, time.Second))
	}
	require.NoError(t, e.svc.Delete(ctx, id))
}

// The keyed locks of snapshots and of image digests do not accumulate.
func TestService_KeyedLocksReturnToEmpty(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	_ = e.svc.DeleteSnapshot(ctx, "never-held")
	_ = e.svc.RemoveImage(ctx, "sha256:never-held")

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := machine.MachineID(fmt.Sprintf("c%d", i))
			_ = e.svc.Create(ctx, id, request(id), baseSpec())
			_ = e.svc.RemoveImage(ctx, image)
			_ = e.svc.DeleteSnapshot(ctx, "snap")
		}()
	}
	wg.Wait()

	snapshots, images := machine.KeyedLockCount(e.svc)
	assert.Zero(t, snapshots, "snapshot locks")
	assert.Zero(t, images, "image locks")
}
