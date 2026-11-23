package catalog_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/catalog"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

var errBoom = errors.New("injected failure")

const (
	used  machine.ImageDigest = "sha256:used"
	spare machine.ImageDigest = "sha256:spare"
)

type node struct {
	images    *memory.ImageStore
	snapshots *memory.SnapshotStore
	store     *memory.StateStore
	catalog   catalog.Service
}

func newNode(t *testing.T) *node {
	t.Helper()
	n := &node{images: memory.NewImageStore(), snapshots: memory.NewSnapshotStore(), store: memory.NewStateStore()}
	n.images.Provide(used)
	n.images.Provide(spare)
	n.catalog = catalog.NewService(n.images, n.snapshots, n.store)
	return n
}

func (n *node) record(t *testing.T, id machine.MachineID, rec machine.Record) {
	t.Helper()
	rec.ID = id
	require.NoError(t, n.store.Save(context.Background(), rec))
}

func (n *node) requireImages(t *testing.T, want ...machine.ImageDigest) {
	t.Helper()
	got, err := n.catalog.ListImages(context.Background())
	require.NoError(t, err)
	var digests []machine.ImageDigest
	for _, info := range got {
		digests = append(digests, info.Digest)
	}
	require.Equal(t, want, digests)
}

func TestListImages_ReportsWhatTheNodeHoldsOrderedByDigest(t *testing.T) {
	n := newNode(t)
	n.images.Provide("sha256:aaa")
	n.requireImages(t, "sha256:aaa", spare, used)
}

func TestListImages_AStoreFailureIsFailedAndNothingIsLost(t *testing.T) {
	n := newNode(t)
	n.images.FailAt("list", errBoom)

	_, err := n.catalog.ListImages(context.Background())

	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	assert.ErrorIs(t, failure.Kind, machine.ErrFailed)
	n.images.Heal("list")
	n.requireImages(t, spare, used)
}

func TestRemoveImage_RemovesAnUnusedImageAndARepeatSucceeds(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)

	require.NoError(t, n.catalog.RemoveImage(ctx, spare))
	n.requireImages(t, used)
	require.NoError(t, n.catalog.RemoveImage(ctx, spare), "an image that is already gone is removed")

	err := n.catalog.RemoveImage(ctx, "")
	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	assert.ErrorIs(t, failure.Kind, machine.ErrInvalid)
}

func TestRemoveImage_RefusesAnImageAMachineRecordNames(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	n.record(t, "m", machine.Record{Machine: &machine.Machine{ID: "m", State: machine.StateStopped, Spec: machine.Spec{Image: used}}})
	n.record(t, "failed", machine.Record{Request: machine.Request{ID: "r", Spec: machine.Spec{Image: spare}, Failure: machine.NewError(machine.ErrFailed, "x")}})

	require.NoError(t, n.catalog.RemoveImage(ctx, spare), "the near miss: a failed attempt's record names no Machine, so its image goes")
	err := n.catalog.RemoveImage(ctx, used)

	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	assert.ErrorIs(t, failure.Kind, machine.ErrPreconditionFailed)
	assert.Contains(t, failure.Reason, `"m"`)
	n.requireImages(t, used)

	require.NoError(t, n.store.Remove(ctx, "m"))
	require.NoError(t, n.catalog.RemoveImage(ctx, used), "once the record is gone nothing names the image")
}

func TestRemoveImage_ALeaseHeldByACreateRefusesTheRemovalAndOtherImagesGo(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	release := n.catalog.HoldImage(used)

	require.NoError(t, n.catalog.RemoveImage(ctx, spare), "the near miss: another image goes")
	err := n.catalog.RemoveImage(ctx, used)
	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	assert.ErrorIs(t, failure.Kind, machine.ErrPreconditionFailed)

	release()
	require.NoError(t, n.catalog.RemoveImage(ctx, used), "control: once the lease is released the removal is made")
}

func TestRemoveImage_AStoreFailureLeavesTheImageAndARepeatRemovesIt(t *testing.T) {
	ctx := context.Background()
	for _, step := range []struct {
		name string
		fail func(*node)
		heal func(*node)
	}{
		{"the image store", func(n *node) { n.images.FailAt("remove", errBoom) }, func(n *node) { n.images.Heal("remove") }},
		{"the Snapshot Store", func(n *node) { n.snapshots.FailAt("list", errBoom) }, func(n *node) { n.snapshots.Heal("list") }},
		{"the State Store", func(n *node) { n.store.FailAt("load", errBoom) }, func(n *node) { n.store.Heal("load") }},
	} {
		t.Run(step.name, func(t *testing.T) {
			n := newNode(t)
			step.fail(n)

			err := n.catalog.RemoveImage(ctx, spare)

			var failure *machine.Error
			require.ErrorAs(t, err, &failure)
			assert.ErrorIs(t, failure.Kind, machine.ErrFailed)
			step.heal(n)
			n.requireImages(t, spare, used)
			require.NoError(t, n.catalog.RemoveImage(ctx, spare))
		})
	}
}

func TestCatalog_LeasesReturnToEmpty(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release := n.catalog.HoldImage(used)
			_ = n.catalog.RemoveImage(ctx, used)
			release()
			_ = n.catalog.RemoveImage(ctx, "sha256:never-held")
		}()
	}
	wg.Wait()

	assert.Zero(t, catalog.ImageLockCount(n.catalog))
}
