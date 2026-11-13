package machine_test

import (
	"bytes"
	"context"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// realDiskService builds the service on the real Disk adapter, whose files the
// tests read back.
func realDiskService(t *testing.T) (machine.Service, *diskfile.Disk) {
	t.Helper()
	disk, err := diskfile.NewDisk(t.TempDir(), "/bin/true")
	require.NoError(t, err)
	e := newEnv(machine.CapabilityDiskHotplug)
	return machine.NewService(e.driver, e.sup, e.net, disk, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels())), disk
}

func TestCreateDiskWithContents_TheFileHoldsTheBytesAtOffsetZero(t *testing.T) {
	ctx := context.Background()
	svc, disk := realDiskService(t)
	contents := bytes.Repeat([]byte("seed"), 300)

	require.NoError(t, svc.CreateDiskWithContents(ctx, "seed", "req", 0, contents))

	path, err := disk.Locate(ctx, "seed")
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, 1536, len(got), "no size is the length rounded up to 512")
	assert.Equal(t, contents, got[:len(contents)])
	assert.Equal(t, make([]byte, len(got)-len(contents)), got[len(contents):], "the rest is zero")
	entry, err := disk.Get(ctx, "seed")
	require.NoError(t, err)
	assert.Equal(t, machine.FilesystemNone, entry.Filesystem)
}

func TestCreateDiskWithContents_ASizeIsKeptAndMustHoldTheBytes(t *testing.T) {
	ctx := context.Background()
	svc, disk := realDiskService(t)
	contents := []byte("0123456789")

	requireKind(t, svc.CreateDiskWithContents(ctx, "small", "req", 9, contents), machine.ErrInvalid)
	_, err := disk.Get(ctx, "small")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent, "the refusal left no disk")

	require.NoError(t, svc.CreateDiskWithContents(ctx, "exact", "req", 10, contents), "control: a size equal to the length")
	require.NoError(t, svc.CreateDiskWithContents(ctx, "big", "req", 4096, contents), "control: a size above it")
	entry, err := disk.Get(ctx, "big")
	require.NoError(t, err)
	assert.Equal(t, uint64(4096), entry.SizeBytes)
}

func TestCreateDiskWithContents_TheLimitIsOneMiB(t *testing.T) {
	ctx := context.Background()
	svc, disk := realDiskService(t)

	requireKind(t, svc.CreateDiskWithContents(ctx, "over", "req", 0, make([]byte, machine.MaxDiskContents+1)), machine.ErrInvalid)
	_, err := disk.Get(ctx, "over")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent, "the refusal left no disk")

	require.NoError(t, svc.CreateDiskWithContents(ctx, "at", "req", 0, make([]byte, machine.MaxDiskContents)), "control: exactly 1 MiB")
}

func TestCreateDiskWithContents_ARepeatIsComparedByContents(t *testing.T) {
	ctx := context.Background()
	svc, disk := realDiskService(t)
	require.NoError(t, svc.CreateDiskWithContents(ctx, "seed", "req", 0, []byte("first")))
	path, err := disk.Locate(ctx, "seed")
	require.NoError(t, err)
	before, err := os.Stat(path)
	require.NoError(t, err)

	require.NoError(t, svc.CreateDiskWithContents(ctx, "seed", "req", 0, []byte("first")), "the same call again")
	requireKind(t, svc.CreateDiskWithContents(ctx, "seed", "req", 0, []byte("other")), machine.ErrConflict)
	// A disk made with no contents is another disk.
	requireKind(t, svc.CreateDisk(ctx, "seed", "req", 512, machine.FilesystemNone), machine.ErrConflict)

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, []byte("first"), got[:5], "the conflict changed nothing")
	after, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, before.ModTime(), after.ModTime(), "the repeat did not rewrite the file")
}
