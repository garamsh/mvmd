package imagestore_test

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func blobsOf(t *testing.T, dir string) []string {
	t.Helper()
	return names(t, filepath.Join(dir, "blobs", "sha256"))
}

func TestStore_ListReportsTheListedImagesOrderedByDigest(t *testing.T) {
	s, _ := newStore(t)
	got, err := s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, got, "a cache that lists none lists none")

	pull(t, s, imageB, content("b"))
	pull(t, s, imageA, content("a"))

	got, err = s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.ImageDigest{imageA, imageB}, got)
}

func TestStore_RemoveUnlistsTheImageAndDeletesTheBlobsOnlyItReached(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	pull(t, s, imageB, content("b"))
	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	require.Len(t, blobsOf(t, dir), 6)

	require.NoError(t, s.Remove(ctx, imageA))

	_, err = s.Locate(ctx, imageA)
	assert.ErrorIs(t, err, machine.ErrImageAbsent)
	assert.NoFileExists(t, located.Path, "the file nothing else reaches is deleted")
	assert.Len(t, blobsOf(t, dir), 3, "the blobs of the image that stays are left")
	_, err = s.Locate(ctx, imageB)
	assert.NoError(t, err, "the other image is still located")
	listed, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.ImageDigest{imageB}, listed)
}

// The near miss of the test above: the EROFS file is the same for both.
func TestStore_RemoveKeepsABlobAnotherListedImageReaches(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("same"))
	pull(t, s, imageB, content("same"))
	located, err := s.Locate(ctx, imageB)
	require.NoError(t, err)
	require.Len(t, blobsOf(t, dir), 5, "the setup: the EROFS is stored once")

	require.NoError(t, s.Remove(ctx, imageA))

	assert.FileExists(t, located.Path, "the shared file stays")
	assert.Len(t, blobsOf(t, dir), 3, "the config and manifest of the removed image are gone")
	_, err = s.Locate(ctx, imageB)
	assert.NoError(t, err, "the image that shares the file is still located, and its verity holds")
}

func TestStore_RemoveOfAnAbsentImageSucceedsAndChangesNothing(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	before := listing(t, dir)

	require.NoError(t, s.Remove(ctx, imageB))
	assert.Equal(t, before, listing(t, dir))
	assert.Len(t, blobsOf(t, dir), 3)

	require.NoError(t, s.Remove(ctx, imageA))
	require.NoError(t, s.Remove(ctx, imageA), "a second removal succeeds")
	assert.Empty(t, blobsOf(t, dir))
}

func TestStore_ARemoveThatFailsBeforeTheSwapLeavesTheImageListedAndItsBlobs(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	old := listing(t, dir)
	s.SetFault(func(step string) error {
		if step == "replace-index" {
			return errors.New("injected")
		}
		return nil
	})

	require.Error(t, s.Remove(ctx, imageA))

	s.SetFault(nil)
	assert.Equal(t, old, listing(t, dir), "index.json was not replaced")
	_, err := s.Locate(ctx, imageA)
	assert.NoError(t, err, "the image is still listed and whole")
	assert.Len(t, blobsOf(t, dir), 3)

	require.NoError(t, s.Remove(ctx, imageA), "a repeat removes it")
	assert.Empty(t, blobsOf(t, dir))
}

func TestStore_RemoveLeavesTheBlobsToTheSweepWhenAListedManifestIsUnreadable(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	pull(t, s, imageB, content("b"))
	// The manifest of the image that stays is gone, so what it reaches is
	// unknown: nothing may be deleted on a guess.
	require.NoError(t, os.Remove(manifestBlobOf(t, dir, imageB)))

	err := s.Remove(ctx, imageA)

	require.Error(t, err, "the removal reports that it did not reclaim")
	assert.Contains(t, err.Error(), "left for the next start's sweep")
	got, lerr := s.List(ctx)
	require.NoError(t, lerr)
	assert.Equal(t, []machine.ImageDigest{imageB}, got, "the image is unlisted")
	assert.Len(t, blobsOf(t, dir), 5, "the blobs the image reached are kept: B's manifest is gone, the rest stay")
}

// manifestBlobOf is the path of the manifest blob the index lists for the
// image.
func manifestBlobOf(t *testing.T, dir string, digest machine.ImageDigest) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
	require.NoError(t, err)
	var idx struct {
		Manifests []struct {
			Digest      string            `json:"digest"`
			Annotations map[string]string `json:"annotations"`
		} `json:"manifests"`
	}
	require.NoError(t, json.Unmarshal(raw, &idx))
	for _, m := range idx.Manifests {
		for _, v := range m.Annotations {
			if v == string(digest) {
				return filepath.Join(dir, "blobs", "sha256", strings.TrimPrefix(m.Digest, "sha256:"))
			}
		}
	}
	require.FailNow(t, "the index lists no manifest for the image")
	return ""
}

// A pull of an image that shares its EROFS file with the image being removed
// is in flight when the removal starts. The Publish found the file in place
// and kept it, so a removal that deleted the file between that and the swap
// would list an image without its file. The two share a mutex: the removal
// waits for the Publish to list the image, then keeps what it reaches.
func TestStore_ARemoveBesideAPublishNeverDeletesABlobThePublishFound(t *testing.T) {
	s, _ := newStore(t)
	pull(t, s, imageA, content("same"))
	staging, _ := stageAndSeal(t, s, imageB, content("same"))

	inPublish := make(chan struct{})
	removeReturned := make(chan struct{})
	removed := make(chan error, 1)
	s.SetFault(func(step string) error {
		if step != "sync-blobs" {
			return nil
		}
		// The Publish has moved its blobs, and found the EROFS in place. A
		// Remove started now gets the window to delete it; the shared mutex
		// makes it wait instead, so give it time to prove which.
		close(inPublish)
		select {
		case <-removeReturned:
		case <-time.After(300 * time.Millisecond):
		}
		return nil
	})
	go func() {
		<-inPublish
		err := s.Remove(ctx, imageA)
		close(removeReturned)
		removed <- err
	}()

	require.NoError(t, s.Publish(ctx, staging), "the file the Publish found is still in place when it syncs it")
	require.NoError(t, <-removed)
	s.SetFault(nil)
	require.NoError(t, s.Discard(ctx, staging))

	_, err := s.Locate(ctx, imageA)
	assert.ErrorIs(t, err, machine.ErrImageAbsent)
	_, err = s.Locate(ctx, imageB)
	assert.NoError(t, err, "the pulled image is listed and its file is whole")
}
