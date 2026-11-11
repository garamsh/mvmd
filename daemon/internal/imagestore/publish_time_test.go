package imagestore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A published image is listed with the time the store published it and the
// blocks its blobs occupy. This test needs fs-verity, so it runs where
// fs-verity is.
func TestStore_PublishStampsTheTimeTheImageIsListedWith(t *testing.T) {
	s, _ := newStore(t)
	at := time.Date(2026, 6, 2, 3, 4, 5, 6, time.UTC)
	s.SetClock(func() time.Time { return at })

	pull(t, s, imageA, content("a"))
	got, err := s.List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.True(t, got[0].CreatedAt.Equal(at), "the time the clock told, got %v", got[0].CreatedAt)
	assert.GreaterOrEqual(t, got[0].SizeBytes, uint64(4096), "the sealed output is in the size")

	s.SetClock(func() time.Time { return at.Add(time.Hour) })
	pull(t, s, imageB, content("b"))
	got, err = s.List(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.True(t, got[0].CreatedAt.Equal(at), "control: the first image keeps its time")
	assert.True(t, got[1].CreatedAt.Equal(at.Add(time.Hour)), "the second has its own")
}
