package machine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// ADR 0072: a disk no Running or Paused Machine holds, or a disk snapshot, is
// read as a stream, and a disk is made from one.

type extent struct {
	offset uint64
	data   []byte
}

// collector is a DiskSink that keeps what a read sent.
type collector struct {
	size    uint64
	fs      machine.Filesystem
	extents []extent
	digest  string
}

func (c *collector) Header(size uint64, fs machine.Filesystem) error {
	c.size, c.fs = size, fs
	return nil
}

func (c *collector) Extent(offset uint64, data []byte) error {
	c.extents = append(c.extents, extent{offset, append([]byte(nil), data...)})
	return nil
}

func (c *collector) Digest(sum string) error {
	c.digest = sum
	return nil
}

// extentList is the extents of a stream.
type extentList []extent

// streamed is a client's stream: the extents, then io.EOF, with the digest the
// sender ended with.
type streamed struct {
	list   extentList
	digest string
}

func (s *streamed) Next() (uint64, []byte, error) {
	if len(s.list) == 0 {
		return 0, nil, io.EOF
	}
	e := s.list[0]
	s.list = s.list[1:]
	return e.offset, e.data, nil
}

func (s *streamed) Digest() string { return s.digest }

// honest is the stream of the extents with the digest of the disk of size bytes
// they stand for.
func honest(size uint64, list extentList) *streamed {
	return &streamed{list: append(extentList(nil), list...), digest: diskSum(logical(size, list))}
}

func diskSum(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// logical lays the extents out as the disk of size bytes they stand for.
func logical(size uint64, extents []extent) []byte {
	out := make([]byte, size)
	for _, e := range extents {
		copy(out[e.offset:], e.data)
	}
	return out
}

const streamBytes = 4 << 20

// sparseDisk is a disk of streamBytes with data in two places and holes
// between and after them.
func sparseDisk() []byte {
	b := make([]byte, streamBytes)
	copy(b[0:], bytes.Repeat([]byte("head"), 2048))
	copy(b[3<<20:], bytes.Repeat([]byte("tail"), 4096))
	return b
}

// realStreamService is the service on the real Disk adapter, with the directory
// the disks are in.
func realStreamService(t *testing.T) (machine.Service, *diskfile.Disk, string) {
	t.Helper()
	dir := t.TempDir()
	disk, err := diskfile.NewDisk(dir, "/bin/true")
	require.NoError(t, err)
	e := newEnv(machine.CapabilityDiskHotplug)
	return machine.NewService(e.driver, e.sup, e.net, disk, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels())), disk, dir
}

// A disk read as a stream and made again from it on another node is the same
// bytes, still sparse, and the digest the read ended with is the one the new disk
// records.
func TestReadDisk_ThenCreateDiskFromStream_IsARoundTrip(t *testing.T) {
	ctx := context.Background()
	source, _, sourceDir := realStreamService(t)
	require.NoError(t, source.CreateDisk(ctx, "d", "r-d", streamBytes, machine.FilesystemNone))
	want := sparseDisk()
	require.NoError(t, os.WriteFile(filepath.Join(sourceDir, "d"), want, 0o600))

	got := &collector{}
	require.NoError(t, source.ReadDisk(ctx, machine.DiskReadSource{Disk: "d"}, got))

	assert.Equal(t, uint64(streamBytes), got.size)
	assert.Equal(t, machine.FilesystemNone, got.fs)
	assert.Equal(t, diskSum(want), got.digest, "the digest is of the whole disk, holes included")
	assert.Equal(t, want, logical(got.size, got.extents), "the extents lay out the disk")
	var sent int
	for _, e := range got.extents {
		sent += len(e.data)
	}
	assert.Less(t, sent, streamBytes/2, "the holes were not sent")

	target, targetDisk, targetDir := realStreamService(t)
	require.NoError(t, target.CreateDiskFromStream(ctx, "copy", "r-c", got.size, got.fs, &streamed{list: got.extents, digest: got.digest}))

	made, err := os.ReadFile(filepath.Join(targetDir, "copy"))
	require.NoError(t, err)
	assert.Equal(t, want, made)
	info, err := os.Stat(filepath.Join(targetDir, "copy"))
	require.NoError(t, err)
	assert.Less(t, uint64(info.Sys().(*syscall.Stat_t).Blocks)*512, uint64(streamBytes/2), "the copy is sparse")
	entry, err := targetDisk.Get(ctx, "copy")
	require.NoError(t, err)
	assert.Equal(t, machine.DiskEntry{ID: "copy", Request: "r-c", SizeBytes: streamBytes, Filesystem: machine.FilesystemNone, ContentsDigest: got.digest}, entry)
}

// A stream with a byte changed, one cut short, and one past the size leave
// nothing on the node; the control is the same stream unchanged, which makes
// the disk.
func TestCreateDiskFromStream_ABadStreamLeavesNothing(t *testing.T) {
	ctx := context.Background()
	want := sparseDisk()
	good := func() extentList {
		return extentList{{0, want[:8192]}, {3 << 20, want[3<<20 : 3<<20+16384]}}
	}
	flipped := good()
	flipped[1].data = append([]byte(nil), flipped[1].data...)
	flipped[1].data[100] ^= 1
	short := good()[:1]
	past := append(good(), extent{streamBytes - 1, []byte("xy")})
	backwards := extentList{{3 << 20, want[3<<20 : 3<<20+16384]}, {0, want[:8192]}}

	for name, list := range map[string]extentList{"a changed byte": flipped, "a missing extent": short, "an extent past the size": past, "extents out of order": backwards} {
		t.Run(name, func(t *testing.T) {
			svc, _, dir := realStreamService(t)

			err := svc.CreateDiskFromStream(ctx, "n", "r", streamBytes, machine.FilesystemNone, &streamed{list: list, digest: diskSum(want)})

			requireKind(t, err, machine.ErrInvalid)
			entries, rerr := os.ReadDir(dir)
			require.NoError(t, rerr)
			assert.Empty(t, entries, "no disk and no temporary file is left")
			all, lerr := svc.ListDisks(ctx)
			require.NoError(t, lerr)
			assert.Empty(t, all)
		})
	}

	svc, disk, _ := realStreamService(t)
	require.NoError(t, svc.CreateDiskFromStream(ctx, "n", "r", streamBytes, machine.FilesystemNone, honest(streamBytes, good())), "control: the stream unchanged makes the disk")
	_, err := disk.Get(ctx, "n")
	require.NoError(t, err)
}

// A retry streams again: the same request ID, size, filesystem and bytes
// succeed; the same bytes under another request ID, size or filesystem, and other
// bytes under the same ones, are a conflict; the control is a new identifier,
// which is made.
func TestCreateDiskFromStream_ARetryStreamsAgainAndAnythingElseIsAConflict(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	data := []byte("abcd")
	size := uint64(4)
	one := func() *streamed { return honest(size, extentList{{0, data}}) }
	require.NoError(t, e.svc.CreateDiskFromStream(ctx, "n", "r", size, machine.FilesystemNone, one()))
	require.Equal(t, 1, e.disks.Calls("create-from-extents"))

	require.NoError(t, e.svc.CreateDiskFromStream(ctx, "n", "r", size, machine.FilesystemNone, one()), "a retry succeeds")
	assert.Equal(t, 2, e.disks.Calls("create-from-extents"), "and the stream was read again")

	requireKind(t, e.svc.CreateDiskFromStream(ctx, "n", "r2", size, machine.FilesystemNone, one()), machine.ErrConflict)
	requireKind(t, e.svc.CreateDiskFromStream(ctx, "n", "r", size, machine.FilesystemExt4, one()), machine.ErrConflict)
	requireKind(t, e.svc.CreateDiskFromStream(ctx, "n", "r", 8, machine.FilesystemNone, honest(8, extentList{{0, data}})), machine.ErrConflict)
	requireKind(t, e.svc.CreateDiskFromStream(ctx, "n", "r", size, machine.FilesystemNone, honest(size, extentList{{0, []byte("zzzz")}})), machine.ErrConflict)

	require.NoError(t, e.svc.CreateDiskFromStream(ctx, "n2", "r", size, machine.FilesystemNone, one()), "control: a new identifier is made")
}

func TestCreateDiskFromStream_RefusesWhatIsNotACall(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	call := func(disk machine.DiskID, request machine.RequestID, size uint64, fs machine.Filesystem) error {
		return e.svc.CreateDiskFromStream(ctx, disk, request, size, fs, honest(1, extentList{{0, []byte("a")}}))
	}
	requireKind(t, call("", "r", 1, machine.FilesystemNone), machine.ErrInvalid)
	requireKind(t, call("tmp-x", "r", 1, machine.FilesystemNone), machine.ErrInvalid)
	requireKind(t, call("n", "", 1, machine.FilesystemNone), machine.ErrInvalid)
	requireKind(t, call("n", "r", 0, machine.FilesystemNone), machine.ErrInvalid)
	requireKind(t, call("n", "r", 1, ""), machine.ErrInvalid)
	requireKind(t, call("n", "r", 1, "btrfs"), machine.ErrInvalid)
	requireKind(t, call("n", "r", machine.MaxDiskBytes+1, machine.FilesystemNone), machine.ErrInvalid)
	assert.Zero(t, e.disks.Calls("create-from-extents"), "a refused call reads no stream")

	require.NoError(t, call("n", "r", 1, machine.FilesystemNone), "control: the same call, valid")
}

// A disk is read when no Machine holds it, and when a Created or Stopped one
// does; a Running, Paused or Faulted holder refuses it and sends nothing.
func TestReadDisk_TheHoldersStateDecides(t *testing.T) {
	for _, tc := range []struct {
		state machine.State
		kind  error
	}{
		{machine.StateCreated, nil},
		{machine.StateStopped, nil},
		{machine.StateRunning, machine.ErrPreconditionFailed},
		{machine.StatePaused, machine.ErrPreconditionFailed},
		{machine.StateFaulted, machine.ErrPreconditionFailed},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			e := newEnv(hotplugOnly...)
			e.withDisk(t, tc.state, mountedAt("d"))
			e.disks.Fill("/disks/d", []byte("contents"))
			got := &collector{}

			err := e.svc.ReadDisk(context.Background(), machine.DiskReadSource{Disk: "d"}, got)

			if tc.kind == nil {
				require.NoError(t, err)
				assert.Equal(t, diskSum([]byte("contents")), got.digest)
				e.requireState(t, "m", tc.state)
				return
			}
			requireKind(t, err, tc.kind)
			assert.Empty(t, got.extents, "nothing was sent")
			assert.Empty(t, got.digest)
			assert.Zero(t, e.disks.Calls("read-extents"))
		})
	}
}

func TestReadDisk_NamesOneExistingSource(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))
	require.NoError(t, snapshotOf(e))
	read := func(s machine.DiskReadSource) error { return e.svc.ReadDisk(ctx, s, &collector{}) }

	requireKind(t, read(machine.DiskReadSource{}), machine.ErrInvalid)
	requireKind(t, read(machine.DiskReadSource{Disk: "d", Snapshot: "s"}), machine.ErrInvalid)
	requireKind(t, read(machine.DiskReadSource{Disk: "a/b"}), machine.ErrInvalid)
	requireKind(t, read(machine.DiskReadSource{Snapshot: "tmp-s"}), machine.ErrInvalid)
	requireKind(t, read(machine.DiskReadSource{Disk: "absent"}), machine.ErrNotFound)
	requireKind(t, read(machine.DiskReadSource{Snapshot: "absent"}), machine.ErrNotFound)
	assert.Zero(t, e.disks.Calls("read-extents"))

	require.NoError(t, read(machine.DiskReadSource{Disk: "d"}), "control: the disk is read")
	require.NoError(t, read(machine.DiskReadSource{Snapshot: "s"}), "control: the snapshot is read")
}

// While a disk no Machine holds is read, nothing takes it: an attach, a delete,
// a resize, a snapshot and another read are refused, and ListDisks reports no
// holder; once the read has ended the disk is free.
func TestReadDisk_WhileTheReadRunsTheDiskIsTaken(t *testing.T) {
	e := newEnv(hotplugOnly...)
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	e.created(t, "m")
	entered, release := e.disks.Hold("read-extents")
	done := make(chan error, 1)
	go func() { done <- e.svc.ReadDisk(ctx, machine.DiskReadSource{Disk: "d"}, &collector{}) }()
	<-entered

	requireKind(t, e.svc.DeleteDisk(ctx, "d"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "d"}), machine.ErrPreconditionFailed)
	requireKind(t, snapshotOf(e), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.ReadDisk(ctx, machine.DiskReadSource{Disk: "d"}, &collector{}), machine.ErrPreconditionFailed)
	all, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	for _, d := range all {
		if d.ID == "d" {
			assert.Empty(t, d.Holder, "the slot the read takes is not a Machine")
		}
	}
	release()
	require.NoError(t, awaitOutcome(t, done))

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: once the read has ended the disk resizes")
}

// A Created Machine holding the disk cannot be started while the disk is read.
func TestReadDisk_AStartWaitsForTheRead(t *testing.T) {
	e := newEnv(hotplugOnly...)
	ctx := context.Background()
	e.withDisk(t, machine.StateCreated, mountedAt("d"))
	entered, release := e.disks.Hold("read-extents")
	done := make(chan error, 1)
	go func() { done <- e.svc.ReadDisk(ctx, machine.DiskReadSource{Disk: "d"}, &collector{}) }()
	<-entered
	started := make(chan error, 1)
	go func() { started <- e.svc.Start(ctx, "m") }()

	select {
	case err := <-started:
		t.Fatalf("Start returned during the read: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	e.requireState(t, "m", machine.StateCreated)
	release()
	require.NoError(t, awaitOutcome(t, done))
	require.NoError(t, awaitOutcome(t, started))
	e.requireState(t, "m", machine.StateRunning)
}

// A disk snapshot is held while it is read: its deletion is refused, and
// allowed once the read has ended.
func TestReadDisk_ASnapshotIsHeldWhileItIsRead(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))
	require.NoError(t, snapshotOf(e))
	e.disks.Fill("/disk-snapshots/s", []byte("snap"))
	entered, release := e.disks.Hold("read-extents")
	done := make(chan error, 1)
	got := &collector{}
	go func() { done <- e.svc.ReadDisk(ctx, machine.DiskReadSource{Snapshot: "s"}, got) }()
	<-entered

	requireKind(t, e.catalog.DeleteDiskSnapshot(ctx, "s"), machine.ErrPreconditionFailed)
	release()
	require.NoError(t, awaitOutcome(t, done))

	assert.Equal(t, diskSum([]byte("snap")), got.digest)
	assert.EqualValues(t, diskBytes, got.size, "the header is the snapshot's")
	require.NoError(t, e.catalog.DeleteDiskSnapshot(ctx, "s"), "control: once the read has ended the snapshot is deleted")
}

// A sink that fails ends the read with its error, and the disk is free again.
func TestReadDisk_ASinkErrorEndsTheReadAndFreesTheDisk(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))
	e.disks.Fill("/disks/d", []byte("contents"))

	err := e.svc.ReadDisk(ctx, machine.DiskReadSource{Disk: "d"}, &failingSink{})

	assert.ErrorIs(t, err, errBoom)
	require.NoError(t, e.svc.DeleteDisk(ctx, "d"), "the disk is not held")
}

type failingSink struct{ collector }

func (*failingSink) Extent(uint64, []byte) error { return errBoom }
