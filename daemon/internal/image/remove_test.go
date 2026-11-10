package image_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The substitute of the Image Store removes as the real one does: it unlists
// the image and reclaims its blobs, and only those, so a later pull of the
// image works.
func TestStore_RemoveUnlistsTheImageAndLeavesTheOthers(t *testing.T) {
	ctx := context.Background()
	r := newRig(t)
	one, two := r.add("one"), r.add("two")
	require.NoError(t, r.pull(one))
	require.NoError(t, r.pull(two))

	require.NoError(t, r.store.Remove(ctx, one.Digest))

	assert.Equal(t, []machine.ImageDigest{two.Digest}, r.store.Listed())
	_, err := r.store.Locate(ctx, one.Digest)
	assert.ErrorIs(t, err, machine.ErrImageAbsent)
	_, err = r.store.Locate(ctx, two.Digest)
	assert.NoError(t, err, "the other image is untouched")
	assert.Zero(t, r.store.Unreferenced(), "the removed image's blobs are gone with it")

	require.NoError(t, r.store.Remove(ctx, one.Digest), "a repeat succeeds")
	require.NoError(t, r.pull(one), "the image can be pulled again")
	assert.ElementsMatch(t, []machine.ImageDigest{one.Digest, two.Digest}, r.store.Listed())
}
