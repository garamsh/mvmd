package imagestore_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/imagestore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A cache written by hand, for what List reads from files, which needs no
// fs-verity: a manifest, a config and one layer per image, and index.json.
type handCache struct {
	t       *testing.T
	dir     string
	entries []map[string]any
}

func newHandCache(t *testing.T) *handCache {
	t.Helper()
	return &handCache{t: t, dir: t.TempDir()}
}

func (c *handCache) blobsDir() string {
	c.t.Helper()
	dir := filepath.Join(c.dir, "blobs", "sha256")
	require.NoError(c.t, os.MkdirAll(dir, 0o700))
	return dir
}

func (c *handCache) blob(b []byte) (string, int) {
	c.t.Helper()
	sum := sha256.Sum256(b)
	h := hex.EncodeToString(sum[:])
	require.NoError(c.t, os.WriteFile(filepath.Join(c.blobsDir(), h), b, 0o600))
	return h, len(b)
}

// add lists an image whose layer is the file write makes; created is the
// annotation, left out when empty. A nil write leaves the layer absent.
func (c *handCache) add(image machine.ImageDigest, write func(path string), created string) {
	c.t.Helper()
	cfgHex, cfgLen := c.blob([]byte(`{"uuid":"` + string(image) + `"}`))
	layerHex := hex.EncodeToString(bytes.Repeat([]byte{byte(len(c.entries) + 1)}, 32))
	if write != nil {
		write(filepath.Join(c.blobsDir(), layerHex))
	}
	m, err := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"config":        map[string]any{"digest": "sha256:" + cfgHex, "size": cfgLen},
		"layers":        []any{map[string]any{"digest": "sha256:" + layerHex, "size": 0}},
	})
	require.NoError(c.t, err)
	mHex, mLen := c.blob(m)
	annotations := map[string]string{"io.mvmd.image.digest": string(image)}
	if created != "" {
		annotations["io.mvmd.image.created"] = created
	}
	c.entries = append(c.entries, map[string]any{
		"mediaType": "application/vnd.oci.image.manifest.v1+json", "digest": "sha256:" + mHex,
		"size": mLen, "annotations": annotations,
	})
}

func (c *handCache) open() *imagestore.Store {
	c.t.Helper()
	doc, err := json.Marshal(map[string]any{
		"schemaVersion": 2, "mediaType": "application/vnd.oci.image.index.v1+json", "manifests": c.entries,
	})
	require.NoError(c.t, err)
	require.NoError(c.t, os.WriteFile(filepath.Join(c.dir, "index.json"), doc, 0o600))
	s, err := imagestore.OpenWithoutVerity(c.dir)
	require.NoError(c.t, err)
	return s
}

func sparseLayer(length int64, data []byte) func(string) {
	return func(path string) {
		f, err := os.Create(path)
		if err != nil {
			panic(err)
		}
		defer f.Close()
		if err := f.Truncate(length); err != nil {
			panic(err)
		}
		if _, err := f.WriteAt(data, 0); err != nil {
			panic(err)
		}
	}
}

func denseLayer(length int) func(string) {
	return func(path string) {
		if err := os.WriteFile(path, bytes.Repeat([]byte{0xab}, length), 0o600); err != nil {
			panic(err)
		}
	}
}

// An image's size is the blocks its blobs occupy, not their length: a sparse
// layer counts what it holds, and the control layer, written whole, counts its
// length.
func TestStore_ListReportsTheBlocksTheImageOccupiesAndNotItsLength(t *testing.T) {
	const length = 64 << 20
	c := newHandCache(t)
	c.add("sha256:sparse", sparseLayer(length, []byte("x")), "")
	c.add("sha256:dense", denseLayer(8<<20), "")

	got, err := c.open().List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 2)
	size := map[machine.ImageDigest]uint64{got[0].Digest: got[0].SizeBytes, got[1].Digest: got[1].SizeBytes}
	assert.Less(t, size["sha256:sparse"], uint64(length/2), "the holes of the layer are not counted")
	assert.Greater(t, size["sha256:sparse"], uint64(0), "the manifest, the config and the data are")
	assert.GreaterOrEqual(t, size["sha256:dense"], uint64(8<<20), "control: a layer written whole counts its length")
}

// When the index records when an image was stored, List reports it; an entry
// without it reads back unset, and a malformed one is an error and not unset.
func TestStore_ListReportsWhenTheImageWasStoredAndNoneForAnEarlierEntry(t *testing.T) {
	stored := time.Date(2026, 5, 1, 12, 30, 0, 123456789, time.UTC)
	c := newHandCache(t)
	c.add("sha256:new", denseLayer(10), stored.Format(time.RFC3339Nano))
	c.add("sha256:old", denseLayer(10), "")

	got, err := c.open().List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.ImageDigest("sha256:new"), got[0].Digest)
	assert.True(t, got[0].CreatedAt.Equal(stored), "control: an entry that records it reports it, got %v", got[0].CreatedAt)
	assert.Equal(t, machine.ImageDigest("sha256:old"), got[1].Digest)
	assert.True(t, got[1].CreatedAt.IsZero(), "an entry written before the time was recorded has none")

	bad := newHandCache(t)
	bad.add("sha256:bad", denseLayer(10), "yesterday")
	_, err = bad.open().List(ctx)
	assert.Error(t, err, "a time that cannot be read is not reported as none")
}

// A layer the cache no longer holds occupies nothing, and the image is still
// listed, with what remains measured.
func TestStore_ListMeasuresAnImageWhoseLayerIsAbsentByWhatRemains(t *testing.T) {
	c := newHandCache(t)
	c.add("sha256:whole", denseLayer(1<<20), "")
	c.add("sha256:cut", nil, "")

	got, err := c.open().List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.ImageDigest("sha256:cut"), got[0].Digest)
	assert.Greater(t, got[0].SizeBytes, uint64(0), "its manifest and config are measured")
	assert.Less(t, got[0].SizeBytes, got[1].SizeBytes, "control: the image with its layer is larger")
}

// A manifest that cannot be read fails the listing: a size measured from part
// of an image is not reported as the image's.
func TestStore_ListFailsWhenAListedManifestCannotBeRead(t *testing.T) {
	c := newHandCache(t)
	c.add("sha256:readable", denseLayer(1<<20), "")
	_, err := c.open().List(ctx)
	require.NoError(t, err, "control: the cache lists while every manifest is readable")

	c.add("sha256:broken", denseLayer(1<<20), "")
	manifest := c.entries[1]["digest"].(string)
	require.NoError(t, os.Remove(filepath.Join(c.blobsDir(), strings.TrimPrefix(manifest, "sha256:"))))

	got, err := c.open().List(ctx)

	assert.Error(t, err, "the listing fails and does not understate the image")
	assert.Nil(t, got, "with no partial list")
}
