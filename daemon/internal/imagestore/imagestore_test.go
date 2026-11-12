package imagestore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/imagestore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

const (
	imageA machine.ImageDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	imageB machine.ImageDigest = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

var ctx = context.Background()

// testClock is the time Publish records in a test that does not set one.
var testClock = memory.NewWallClock(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))

func newStore(t *testing.T) (*imagestore.Store, string) {
	t.Helper()
	dir := verityDir(t)
	s, err := imagestore.New(dir, testClock)
	require.NoError(t, err)
	return s, dir
}

// content is an EROFS stand-in of one block and a tag, so two images differ
// by it.
func content(tag string) []byte {
	return append(bytes.Repeat([]byte{'x'}, 4096), tag...)
}

// stageAndSeal runs a pull up to Record: stage, write the output, seal and
// record.
func stageAndSeal(t *testing.T, s *imagestore.Store, digest machine.ImageDigest, data []byte) (image.Staging, image.Record) {
	t.Helper()
	return stageAndSealRun(t, s, digest, data, machine.ImageRun{Entrypoint: []string{"/bin/app"}, Cmd: []string{"--serve"}, Env: map[string]string{"A": "b"}, WorkingDir: "/srv", User: "app:grp"})
}

func stageAndSealRun(t *testing.T, s *imagestore.Store, digest machine.ImageDigest, data []byte, run machine.ImageRun) (image.Staging, image.Record) {
	t.Helper()
	staging, err := s.Stage(ctx, digest, uint64(len(data))+8192)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(staging.Output, data, 0o600))
	verity, err := s.Seal(ctx, staging, uint64(len(data)))
	require.NoError(t, err)
	record := image.Record{
		Image:        digest,
		Run:          run,
		Architecture: "amd64",
		Converter:    "converter-1",
		UUID:         "uuid-of-" + string(digest)[7:15],
		Verity:       verity,
	}
	require.NoError(t, s.Record(ctx, staging, record))
	return staging, record
}

// pull runs a whole pull of the image and returns its record.
func pull(t *testing.T, s *imagestore.Store, digest machine.ImageDigest, data []byte) image.Record {
	t.Helper()
	staging, record := stageAndSeal(t, s, digest, data)
	require.NoError(t, s.Publish(ctx, staging))
	require.NoError(t, s.Discard(ctx, staging))
	return record
}

func listing(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "index.json"))
	require.NoError(t, err)
	return string(b)
}

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return out
}

func TestStore_LocatesAPublishedImageOnlyAfterPublish(t *testing.T) {
	s, dir := newStore(t)
	data := content("a")

	staging, record := stageAndSeal(t, s, imageA, data)
	_, err := s.Locate(ctx, imageA)
	assert.ErrorIs(t, err, machine.ErrImageAbsent, "control: a recorded image is not visible before Publish")

	require.NoError(t, s.Publish(ctx, staging))
	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	assert.Equal(t, record.UUID, located.UUID)
	assert.Equal(t, record.Run, located.Run)
	assert.True(t, strings.HasPrefix(located.Path, filepath.Join(dir, "blobs", "sha256")+string(filepath.Separator)))
	onDisk, err := os.ReadFile(located.Path)
	require.NoError(t, err)
	assert.Equal(t, data, onDisk, "the file holds the output truncated to the sealed length")
	assert.Regexp(t, `^sha256:[0-9a-f]{64}$`, record.Verity)
}

// The VMM's user is not the daemon, so the image is readable by everyone and
// writable by no one (ADR 0015). The staged output is 0600 until Publish.
func TestStore_PublishedImageIsReadableByEveryUserAndWritableByNone(t *testing.T) {
	s, _ := newStore(t)
	staging, _ := stageAndSeal(t, s, imageA, content("a"))
	_, _, mode := ownerOf(t, staging.Output)
	assert.Equal(t, os.FileMode(0o600), mode, "control: the sealed output keeps its mode until Publish")

	require.NoError(t, s.Publish(ctx, staging))

	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	uid, _, mode := ownerOf(t, located.Path)
	assert.Equal(t, os.FileMode(0o444), mode)
	assert.Equal(t, uint32(os.Geteuid()), uid, "the daemon still owns the image")
}

// A cache published before the mode was set holds images the VMM's user cannot
// open, and New puts that right.
func TestNew_SetsTheModeOfTheImagesOfAnEarlierRun(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(located.Path, 0o600), "setup: the mode of an image published earlier")

	_, err = imagestore.New(dir, testClock)
	require.NoError(t, err)

	_, _, mode := ownerOf(t, located.Path)
	assert.Equal(t, os.FileMode(0o444), mode)
}

func TestStore_LocateRefusesAFileWhoseVerityDiffers(t *testing.T) {
	tests := []struct {
		name    string
		replace func(t *testing.T, path string)
		reason  string
	}{
		{"a file without fs-verity", func(t *testing.T, path string) {
			replaceWith(t, path, content("a"), false)
		}, "not a verity file"},
		{"a verity file with other bytes", func(t *testing.T, path string) {
			replaceWith(t, path, content("other"), true)
		}, "recorded as"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newStore(t)
			pull(t, s, imageA, content("a"))
			before, err := s.Locate(ctx, imageA)
			require.NoError(t, err, "control: the published file is accepted")

			tt.replace(t, before.Path)
			_, err = s.Locate(ctx, imageA)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.reason)
			assert.NotErrorIs(t, err, machine.ErrImageAbsent, "the image is listed; it is refused, not absent")
		})
	}
}

// replaceWith puts a file with the bytes at path, with fs-verity enabled
// where asked, by renaming it over the old one.
func replaceWith(t *testing.T, path string, data []byte, verity bool) {
	t.Helper()
	if verity {
		other, err := imagestore.New(verityDir(t), testClock)
		require.NoError(t, err)
		staging, _ := stageAndSeal(t, other, imageB, data)
		require.NoError(t, os.Rename(staging.Output, path))
		return
	}
	tmp := path + ".plain"
	require.NoError(t, os.WriteFile(tmp, data, 0o600))
	require.NoError(t, os.Rename(tmp, path))
}

func TestNew_RefusesAFileSystemWithoutVerity(t *testing.T) {
	_, err := imagestore.New(verityDir(t), testClock)
	require.NoError(t, err, "control: the file system the tests found accepts the cache")

	shm, err := os.MkdirTemp("/dev/shm", "mvmd-no-verity-")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(shm) })
	_, err = imagestore.New(shm, testClock)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "fs-verity")
	assert.Contains(t, err.Error(), "will not start")
}

func TestNew_KeepsTheImagesOfAnEarlierRun(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))

	reopened, err := imagestore.New(dir, testClock)
	require.NoError(t, err)
	_, err = reopened.Locate(ctx, imageA)
	assert.NoError(t, err)
	_, err = reopened.Locate(ctx, imageB)
	assert.ErrorIs(t, err, machine.ErrImageAbsent, "control: an image never pulled is absent")
	assert.FileExists(t, filepath.Join(dir, "oci-layout"))
}

func TestStore_AFailedStepBeforePublishLeavesTheListingUnchanged(t *testing.T) {
	failing := errors.New("read failed")
	steps := []struct {
		name string
		run  func(t *testing.T, s *imagestore.Store) error
	}{
		{"put-layer", func(t *testing.T, s *imagestore.Store) error {
			staging, err := s.Stage(ctx, imageB, 8192)
			require.NoError(t, err)
			_, err = s.PutLayer(ctx, staging, 0, failingReader{failing})
			return err
		}},
		{"seal", func(t *testing.T, s *imagestore.Store) error {
			staging, err := s.Stage(ctx, imageB, 8192)
			require.NoError(t, err)
			require.NoError(t, os.Remove(staging.Output))
			_, err = s.Seal(ctx, staging, 4096)
			return err
		}},
		{"record", func(t *testing.T, s *imagestore.Store) error {
			staging, _ := stageAndSeal(t, s, imageB, content("b"))
			return s.Record(ctx, staging, image.Record{Image: imageB, Verity: "sha256:" + strings.Repeat("0", 64)})
		}},
		{"publish", func(t *testing.T, s *imagestore.Store) error {
			staging, err := s.Stage(ctx, imageB, 8192)
			require.NoError(t, err)
			return s.Publish(ctx, staging)
		}},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			s, dir := newStore(t)
			pull(t, s, imageA, content("a"))
			before := listing(t, dir)

			require.Error(t, tt.run(t, s))
			assert.Equal(t, before, listing(t, dir))
			_, err := s.Locate(ctx, imageB)
			assert.ErrorIs(t, err, machine.ErrImageAbsent)
			_, err = s.Locate(ctx, imageA)
			assert.NoError(t, err, "control: the image published before is still located")
		})
	}
}

type failingReader struct{ err error }

func (r failingReader) Read([]byte) (int, error) { return 0, r.err }

func TestStore_PublishFailureAtEachStepLeavesTheOldIndexOrTheNew(t *testing.T) {
	recordSteps := func(t *testing.T) []string {
		s, _ := newStore(t)
		pull(t, s, imageA, content("a"))
		var seen []string
		s.SetFault(func(step string) error { seen = append(seen, step); return nil })
		pull(t, s, imageB, content("b"))
		return seen
	}
	steps := recordSteps(t)
	require.Equal(t, []string{"move-blob", "move-blob", "move-blob", "sync-blobs", "write-index", "sync-index", "replace-index", "sync-dir"}, steps,
		"the steps of Publish, in the order ADR 0007 states them")

	for at, step := range steps {
		t.Run(fmt.Sprintf("%d-%s", at, step), func(t *testing.T) {
			s, dir := newStore(t)
			pull(t, s, imageA, content("a"))
			old := listing(t, dir)
			staging, _ := stageAndSeal(t, s, imageB, content("b"))
			calls := 0
			s.SetFault(func(string) error {
				calls++
				if calls == at+1 {
					return errors.New("injected")
				}
				return nil
			})

			require.Error(t, s.Publish(ctx, staging))
			s.SetFault(nil)

			current := listing(t, dir)
			_, errB := s.Locate(ctx, imageB)
			if step == "sync-dir" {
				assert.NotEqual(t, old, current, "the rename was done, so the new index is in place")
				assert.NoError(t, errB, "Publish returned an error with the image visible, so image.Store leaves the caller to ask Locate")
			} else {
				assert.Equal(t, old, current, "nothing before the rename touches index.json")
				assert.ErrorIs(t, errB, machine.ErrImageAbsent)
			}
			_, err := s.Locate(ctx, imageA)
			assert.NoError(t, err, "the image listed before stays located")

			require.NoError(t, s.Discard(ctx, staging))
			require.NoError(t, s.Sweep(ctx))
			assert.Empty(t, names(t, filepath.Join(dir, "staging")))
			assert.NoFileExists(t, filepath.Join(dir, "index.json.tmp"))
			wantBlobs := 3
			if errB == nil {
				wantBlobs = 6
			}
			assert.Len(t, names(t, filepath.Join(dir, "blobs", "sha256")), wantBlobs, "only what the listed images reach is left")
		})
	}
}

func TestStore_SweepRemovesWhatACrashLeft(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	listed := names(t, filepath.Join(dir, "blobs", "sha256"))

	_, err := s.Stage(ctx, imageB, 8192)
	require.NoError(t, err)
	stray := filepath.Join(dir, "blobs", "sha256", strings.Repeat("c", 64))
	require.NoError(t, os.WriteFile(stray, []byte("left"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json.tmp"), []byte("{"), 0o600))
	require.Len(t, names(t, filepath.Join(dir, "staging")), 1, "setup left a staging area")

	require.NoError(t, s.Sweep(ctx))
	assert.Empty(t, names(t, filepath.Join(dir, "staging")))
	assert.NoFileExists(t, stray)
	assert.NoFileExists(t, filepath.Join(dir, "index.json.tmp"))
	assert.Equal(t, listed, names(t, filepath.Join(dir, "blobs", "sha256")), "control: the listed image's blobs stay")
	_, err = s.Locate(ctx, imageA)
	assert.NoError(t, err)
}

func TestStore_SweepRemovesNothingWhenAListedManifestCannotBeRead(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	blobs := filepath.Join(dir, "blobs", "sha256")
	stray := filepath.Join(blobs, strings.Repeat("c", 64))
	require.NoError(t, os.WriteFile(stray, []byte("left"), 0o600))

	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	for _, n := range names(t, blobs) {
		if filepath.Join(blobs, n) != located.Path && filepath.Join(blobs, n) != stray && isManifest(t, filepath.Join(blobs, n)) {
			require.NoError(t, os.Remove(filepath.Join(blobs, n)))
		}
	}

	require.Error(t, s.Sweep(ctx))
	assert.FileExists(t, stray, "what the listed image reaches is unknown, so nothing is removed")
	assert.FileExists(t, located.Path)
}

func isManifest(t *testing.T, path string) bool {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return bytes.Contains(b, []byte("artifactType"))
}

func TestStore_IdenticalBlobsAreStoredOnce(t *testing.T) {
	s, dir := newStore(t)
	blobs := filepath.Join(dir, "blobs", "sha256")
	pull(t, s, imageA, content("same"))
	assert.Len(t, names(t, blobs), 3)

	pull(t, s, imageB, content("same"))
	assert.Len(t, names(t, blobs), 5, "the EROFS is stored once; each image has a config and a manifest of its own")

	a, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	b, err := s.Locate(ctx, imageB)
	require.NoError(t, err)
	assert.Equal(t, a.Path, b.Path)
}

func TestStore_RefusesCallsOutOfOrderAndForeignStagingAreas(t *testing.T) {
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)

	assert.ErrorContains(t, s.Record(ctx, staging, image.Record{Image: imageA}), "before seal")
	assert.ErrorContains(t, s.Publish(ctx, staging), "before record")

	for _, id := range []string{"../escape", "", "ABC"} {
		foreign := image.Staging{ID: id}
		_, err := s.Seal(ctx, foreign, 4096)
		assert.Error(t, err, "Seal of %q", id)
		assert.Error(t, s.Discard(ctx, foreign), "Discard of %q", id)
	}
	_, err = s.Seal(ctx, image.Staging{ID: strings.Repeat("0", 32)}, 4096)
	assert.ErrorContains(t, err, "no staging area", "a well-formed identifier this store never issued")
	assert.NoError(t, s.Discard(ctx, staging), "control: its own staging area is discarded")
}

func TestStore_StageCreatesTheOutputSparseAtTheSize(t *testing.T) {
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 1<<30)
	require.NoError(t, err)
	info, err := os.Stat(staging.Output)
	require.NoError(t, err)
	assert.Equal(t, int64(1<<30), info.Size())
}

// A record written before the entrypoint, the command and the user were kept
// apart reads as its joined vector in the entrypoint, with the marker.
func TestStore_LocatesARecordInTheOldFormMarked(t *testing.T) {
	s, dir := newStore(t)
	pull(t, s, imageA, content("a"))
	fresh, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	require.False(t, fresh.Run.Joined, "the setup: a new record is not marked")
	rewriteRunAsOldForm(t, dir, imageA)

	old, err := s.Locate(ctx, imageA)
	require.NoError(t, err)

	assert.Equal(t, machine.ImageRun{Entrypoint: []string{"/bin/app", "--serve"}, Env: map[string]string{"A": "b"}, WorkingDir: "/srv", Joined: true}, old.Run)
}

// ReplaceRun gives an old-form record the split run and keeps the image's
// file, UUID and verity.
func TestStore_ReplaceRunKeepsTheImageAndReplacesTheOldFormRecord(t *testing.T) {
	s, dir := newStore(t)
	record := pull(t, s, imageA, content("a"))
	rewriteRunAsOldForm(t, dir, imageA)
	old, err := s.Locate(ctx, imageA)
	require.NoError(t, err)
	require.True(t, old.Run.Joined, "the setup: the record is in the old form")
	onDisk, err := os.ReadFile(old.Path)
	require.NoError(t, err)

	require.NoError(t, s.ReplaceRun(ctx, imageA, record.Run))

	located, err := s.Locate(ctx, imageA)
	require.NoError(t, err, "the file still passes the fs-verity check Locate makes")
	assert.Equal(t, record.Run, located.Run)
	assert.Equal(t, old.Path, located.Path)
	assert.Equal(t, old.UUID, located.UUID)
	again, err := os.ReadFile(located.Path)
	require.NoError(t, err)
	assert.Equal(t, onDisk, again)

	// The control: an image the cache does not list is absent.
	assert.ErrorIs(t, s.ReplaceRun(ctx, imageB, record.Run), machine.ErrImageAbsent)
}

// rewriteRunAsOldForm replaces the image's config blob with one whose run is in
// the old form, and re-points the manifest and the index at it.
func rewriteRunAsOldForm(t *testing.T, dir string, digest machine.ImageDigest) {
	t.Helper()
	blobs := filepath.Join(dir, "blobs", "sha256")
	readJSON := func(path string) map[string]any {
		b, err := os.ReadFile(path)
		require.NoError(t, err)
		var m map[string]any
		require.NoError(t, json.Unmarshal(b, &m))
		return m
	}
	put := func(m map[string]any) (string, int) {
		b, err := json.Marshal(m)
		require.NoError(t, err)
		sum := sha256.Sum256(b)
		h := hex.EncodeToString(sum[:])
		require.NoError(t, os.WriteFile(filepath.Join(blobs, h), b, 0o644))
		return "sha256:" + h, len(b)
	}
	index := readJSON(filepath.Join(dir, "index.json"))
	var entry map[string]any
	for _, e := range index["manifests"].([]any) {
		cand := e.(map[string]any)
		if cand["annotations"].(map[string]any)["io.mvmd.image.digest"] == string(digest) {
			entry = cand
		}
	}
	require.NotNil(t, entry, "the setup: the index lists the image")

	manifest := readJSON(filepath.Join(blobs, strings.TrimPrefix(entry["digest"].(string), "sha256:")))
	cfgDesc := manifest["config"].(map[string]any)
	cfg := readJSON(filepath.Join(blobs, strings.TrimPrefix(cfgDesc["digest"].(string), "sha256:")))
	run := cfg["run"].(map[string]any)
	joined := append(run["entrypoint"].([]any), run["cmd"].([]any)...)
	cfg["run"] = map[string]any{"command": joined, "env": run["env"], "workingDir": run["workingDir"]}

	cfgDesc["digest"], cfgDesc["size"] = put(cfg)
	entry["digest"], entry["size"] = put(manifest)
	b, err := json.Marshal(index)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "index.json"), b, 0o644))
}
