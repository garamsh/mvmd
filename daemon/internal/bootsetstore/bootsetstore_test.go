package bootsetstore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/bootsetstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
)

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func upload(kernel, initrd string) machine.BootUpload {
	return machine.BootUpload{
		Kernel: machine.BootContent{SHA256: hexSum(kernel), Content: bytes.NewReader([]byte(kernel))},
		Initrd: machine.BootContent{SHA256: hexSum(initrd), Content: bytes.NewReader([]byte(initrd))},
	}
}

func open(t *testing.T, dir string) *bootsetstore.Store {
	t.Helper()
	s, err := bootsetstore.New(dir)
	require.NoError(t, err)
	return s
}

func TestStore_AnAddedSetHoldsItsBytesReadOnlyAndSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "boot-sets")
	s := open(t, dir)
	require.NoError(t, s.Add(ctx, "mine", upload("kernel-a", "initrd-a")))

	for _, store := range []*bootsetstore.Store{s, open(t, dir)} {
		set, err := store.Get(ctx, "mine")
		require.NoError(t, err)
		assert.Equal(t, hexSum("kernel-a"), set.Kernel.SHA256)
		assert.Equal(t, hexSum("initrd-a"), set.Initrd.SHA256)
		require.NoError(t, pinnedfile.Check("the kernel", set.Kernel.Path, set.Kernel.SHA256))
		require.NoError(t, pinnedfile.Check("the initrd", set.Initrd.Path, set.Initrd.SHA256))
		listed, err := store.List(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 1)
	}

	set, err := s.Get(ctx, "mine")
	require.NoError(t, err)
	for _, p := range []string{set.Kernel.Path, set.Initrd.Path} {
		info, err := os.Stat(p)
		require.NoError(t, err)
		assert.Zero(t, info.Mode().Perm()&0o222, "%s is read-only", p)
	}
}

func TestStore_AFileChangedAfterTheAddFailsItsCheck(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "boot-sets"))
	require.NoError(t, s.Add(ctx, "mine", upload("kernel-a", "initrd-a")))
	set, err := s.Get(ctx, "mine")
	require.NoError(t, err)
	require.NoError(t, pinnedfile.Check("the kernel", set.Kernel.Path, set.Kernel.SHA256), "control: untouched")

	require.NoError(t, os.Chmod(set.Kernel.Path, 0o600))
	require.NoError(t, os.WriteFile(set.Kernel.Path, []byte("kernel-b"), 0o600))
	assert.Error(t, pinnedfile.Check("the kernel", set.Kernel.Path, set.Kernel.SHA256))
}

func TestStore_AddRefusesABadFileAndPublishesNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		file   string
		change func(*machine.BootUpload)
	}{
		{"kernel", func(u *machine.BootUpload) { u.Kernel.SHA256 = hexSum("other") }},
		{"initrd", func(u *machine.BootUpload) { u.Initrd.SHA256 = hexSum("other") }},
	} {
		t.Run(tc.file, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "boot-sets")
			s := open(t, dir)
			u := upload("kernel-a", "initrd-a")
			tc.change(&u)
			err := s.Add(ctx, "mine", u)
			var mismatch *machine.BootFileMismatch
			require.ErrorAs(t, err, &mismatch)
			assert.Equal(t, tc.file, mismatch.File)

			_, err = s.Get(ctx, "mine")
			assert.ErrorIs(t, err, machine.ErrBootSetAbsent)
			left, err := os.ReadDir(filepath.Join(dir, ".staging"))
			require.NoError(t, err)
			assert.Empty(t, left, "no staged files remain")
			require.NoError(t, s.Add(ctx, "mine", upload("kernel-a", "initrd-a")), "control: the right digests are accepted")
		})
	}
}

func TestStore_AddOfAHeldNameIsRefusedAndKeepsTheSet(t *testing.T) {
	ctx := context.Background()
	s := open(t, filepath.Join(t.TempDir(), "boot-sets"))
	require.NoError(t, s.Add(ctx, "mine", upload("kernel-a", "initrd-a")))
	assert.ErrorIs(t, s.Add(ctx, "mine", upload("kernel-b", "initrd-b")), machine.ErrBootSetExists)
	set, err := s.Get(ctx, "mine")
	require.NoError(t, err)
	assert.Equal(t, hexSum("kernel-a"), set.Kernel.SHA256)
}

func TestStore_RemoveDeletesTheFilesAndAnAbsentNameIsSuccess(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "boot-sets")
	s := open(t, dir)
	require.NoError(t, s.Add(ctx, "mine", upload("kernel-a", "initrd-a")))
	require.NoError(t, s.Add(ctx, "other", upload("kernel-b", "initrd-b")))

	require.NoError(t, s.Remove(ctx, "mine"))
	_, err := s.Get(ctx, "mine")
	assert.ErrorIs(t, err, machine.ErrBootSetAbsent)
	_, err = os.Stat(filepath.Join(dir, "mine"))
	assert.ErrorIs(t, err, os.ErrNotExist)
	_, err = s.Get(ctx, "other")
	require.NoError(t, err, "control: the other set stays")
	require.NoError(t, s.Remove(ctx, "mine"))
}

func TestNew_RemovesWhatACrashLeftInStaging(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "boot-sets")
	open(t, dir)
	left := filepath.Join(dir, ".staging", "add-1")
	require.NoError(t, os.MkdirAll(left, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(left, "kernel"), []byte("half"), 0o444))

	s := open(t, dir)
	_, err := os.Stat(left)
	assert.ErrorIs(t, err, os.ErrNotExist)
	listed, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, listed)
}
