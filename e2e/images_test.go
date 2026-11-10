//go:build e2e_real

package e2e_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// List images lists every image the node holds, by the digest each was pulled by and ordered by
// it, and none before the first pull.
func TestE2E_ListImagesListsThePulledImagesByDigest(t *testing.T) {
	n := startNode(t)
	// The control: the node held no image before the test pulled these.
	require.Empty(t, n.listedImages(t))

	first := n.pullBootable(t, "listed-first")
	second := n.pullBootable(t, "listed-second", layer(file("second-only", "x")))
	require.NotEqual(t, first.Digest, second.Digest, "two images that differ in a layer are two digests")

	want := []string{first.Digest, second.Digest}
	slices.Sort(want)
	assert.Equal(t, want, n.listedImages(t), "the digests the images were pulled by, in order")
}

// Remove refuses an image a Machine references, whatever its state, and leaves it listed and its
// files in place; it removes an image no Machine references, with the files only that image
// reached, and leaves the files of the other; a repeat that finds the image absent succeeds.
func TestE2E_RemoveImageRefusesAnImageAMachineUsesAndRemovesAnotherWithItsFiles(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	used := n.pullBootable(t, "used")
	unused := n.pullBootable(t, "unused", layer(file("unused-only", "x")))
	usedFile, unusedFile := n.erofsOf(t, used.Digest), n.erofsOf(t, unused.Digest)
	require.NotEqual(t, usedFile, unusedFile, "each image has a file of its own, so removing one is told from removing both")
	_, err := n.client.CreateMachine(ctx, createRequest("user", used.Digest))
	require.NoError(t, err)

	err = n.removeImage(t, ctx, used.Digest)
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "an image a Machine references")
	assert.Contains(t, n.listedImages(t), used.Digest, "the refused image is still listed")
	assert.FileExists(t, usedFile, "the refused image's file is still there")

	// The control: the refusal is the Machine's, so an image no Machine references is removed
	// by the same call.
	require.NoError(t, n.removeImage(t, ctx, unused.Digest))
	assert.Equal(t, []string{used.Digest}, n.listedImages(t), "the removed image is not listed and the other is")
	_, err = os.Stat(unusedFile)
	assert.True(t, errors.Is(err, fs.ErrNotExist), "the file only the removed image reached is deleted: %v", err)
	assert.FileExists(t, usedFile, "the file of the image that stayed")

	assert.NoError(t, n.removeImage(t, ctx, unused.Digest), "an image that is absent is removed with no change")
}

// Remove refuses an image a snapshot references once no Machine does, and removes it after
// Delete snapshot.
func TestE2E_RemoveImageRefusesAnImageASnapshotReferencesUntilTheSnapshotIsDeleted(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "snapshotted")
	n.runMachine(t, ctx, createRequest("origin", pulled.Digest))
	_, err := n.client.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "snapshot-snap", MachineId: "origin"})
	require.NoError(t, err)
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "origin", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "origin"})
	require.NoError(t, err)

	// No Machine references the image now, so the snapshot is what refuses.
	err = n.removeImage(t, ctx, pulled.Digest)
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "an image a snapshot references")
	require.Equal(t, []string{pulled.Digest}, n.listedImages(t), "the refused image is still listed")

	_, err = n.client.DeleteSnapshot(ctx, &v1alpha2.DeleteSnapshotRequest{SnapshotId: "snap"})
	require.NoError(t, err)
	require.NoError(t, n.removeImage(t, ctx, pulled.Digest))
	assert.Empty(t, n.listedImages(t), "the image is removed once nothing references it")
}

// listedImages is the digests List images answers, in the order it answers them.
func (n *node) listedImages(t *testing.T) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	listed, err := n.client.ListImages(ctx, &v1alpha2.ListImagesRequest{})
	require.NoError(t, err)
	var digests []string
	for _, image := range listed.Images {
		digests = append(digests, image.Digest)
	}
	return digests
}

// removeImage is the Remove call, which the test reads the outcome of.
func (n *node) removeImage(t *testing.T, ctx context.Context, digest string) error {
	t.Helper()
	_, err := n.client.RemoveImage(ctx, &v1alpha2.RemoveImageRequest{Digest: digest})
	return err
}
