package diskfile_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

type run struct {
	offset uint64
	data   []byte
}

// runs are the extents of a stream, and the digest the sender ends it with.
type runs struct {
	rs     []run
	digest string
}

func (r *runs) Next() (uint64, []byte, error) {
	if len(r.rs) == 0 {
		return 0, nil, io.EOF
	}
	n := r.rs[0]
	r.rs = r.rs[1:]
	return n.offset, n.data, nil
}

func (r *runs) Digest() string { return r.digest }

// stream is the extents with the digest of the bytes a reader sees.
func stream(size int, rs ...run) *runs {
	return &runs{rs: rs, digest: sumOf(logicalOf(size, rs...))}
}

func sumOf(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// logicalOf is the bytes a reader of a disk of size sees, the runs over zeros.
func logicalOf(size int, rs ...run) []byte {
	b := make([]byte, size)
	for _, r := range rs {
		copy(b[r.offset:], r.data)
	}
	return b
}

func entryOf(size uint64) machine.DiskEntry {
	return machine.DiskEntry{ID: "d", Request: "r", SizeBytes: size, Filesystem: machine.FilesystemNone}
}

func TestCreateFromExtents_HolesAndZeroBlocksAreZerosInTheDigest(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	const size = 3 << 20
	rs := []run{{offset: 5, data: []byte("head")}, {offset: 1 << 20, data: make([]byte, 4096)}, {offset: 2<<20 + 17, data: []byte("tail")}}

	digest, err := d.CreateFromExtents(ctx, entryOf(size), stream(size, rs...))

	require.NoError(t, err)
	assert.Equal(t, sumOf(logicalOf(size, rs...)), digest)
	got, err := os.ReadFile(filepath.Join(dir, "d"))
	require.NoError(t, err)
	assert.Equal(t, logicalOf(size, rs...), got)
	have, err := d.Get(ctx, "d")
	require.NoError(t, err)
	want := entryOf(size)
	want.ContentsDigest = digest
	assert.Equal(t, want, have, "the entry records the digest the stream was checked against")
}

func TestCreateFromExtents_ABadStreamLeavesNoFileAndNoTemporary(t *testing.T) {
	good := []run{{offset: 0, data: []byte("0123456789abcdef")}}
	crafted := &runs{rs: []run{{0, []byte("AAAA")}, {2, []byte("BB")}}, digest: sumOf([]byte("AAAABB"))}
	for name, bad := range map[string]*runs{
		"a changed byte":   {rs: []run{{0, []byte("0123456789abcdeX")}}, digest: sumOf(good[0].data)},
		"a missing extent": {rs: []run{{0, []byte("01234567")}}, digest: sumOf(good[0].data)},
		"no digest":        {rs: good},
		// The digest of the bytes in the order they arrive, which an overlap
		// writes as "AABB" under the digest of "AAAABB": only the order check
		// refuses it.
		"an overlap under the digest of the arrival order": crafted,
		"past the size":        {rs: []run{{8, []byte("0123456789")}}, digest: sumOf(good[0].data)},
		"out of order":         {rs: []run{{8, []byte("89abcdef")}, {0, []byte("01234567")}}, digest: sumOf(good[0].data)},
		"overlapping":          {rs: []run{{0, []byte("0123456789")}, {8, []byte("89abcdef")}}, digest: sumOf(good[0].data)},
		"an offset that wraps": {rs: []run{{^uint64(0) - 2, []byte("0123456789abcdef")}}, digest: sumOf(good[0].data)},
	} {
		t.Run(name, func(t *testing.T) {
			d, dir := newDisk(t)
			size := uint64(16)
			if bad == crafted {
				size = 6
			}

			_, err := d.CreateFromExtents(context.Background(), entryOf(size), bad)

			assert.ErrorIs(t, err, machine.ErrDiskStreamMismatch)
			entries, rerr := os.ReadDir(dir)
			require.NoError(t, rerr)
			assert.Empty(t, entries, "no disk and no temporary file")
			_, err = d.CreateFromExtents(context.Background(), entryOf(16), stream(16, good...))
			require.NoError(t, err, "control: the right stream is accepted")
		})
	}
}

func TestCreateFromExtents_ADeadlineEndsTheHashingOfAHugeGap(t *testing.T) {
	d, dir := newDisk(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()

	// 8 GiB of zeros take seconds to hash, and a size a stream may claim is far
	// larger.
	_, err := d.CreateFromExtents(ctx, entryOf(8<<30), &runs{digest: sumOf(nil)})

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second, "the call ended with its context, not with the hash")
	entries, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	assert.Empty(t, entries, "no disk and no temporary file")
	_, err = d.CreateFromExtents(context.Background(), entryOf(1<<20), stream(1<<20))
	require.NoError(t, err, "control: a small disk of zeros is accepted")
}

func TestCreateFromExtents_ARepeatIsErrDiskExistsWithTheDigestItVerified(t *testing.T) {
	d, _ := newDisk(t)
	ctx := context.Background()
	first, err := d.CreateFromExtents(ctx, entryOf(4), stream(4, run{0, []byte("abcd")}))
	require.NoError(t, err)

	second, err := d.CreateFromExtents(ctx, entryOf(4), stream(4, run{0, []byte("abcd")}))

	assert.ErrorIs(t, err, machine.ErrDiskExists)
	assert.Equal(t, first, second)
	other, err := d.CreateFromExtents(ctx, entryOf(4), stream(4, run{0, []byte("abce")}))
	assert.ErrorIs(t, err, machine.ErrDiskExists)
	assert.NotEqual(t, first, other, "the digest of other bytes is told apart")
}

func TestCreateFromExtents_AnExtentAcrossABlockBoundaryIsKeptWhole(t *testing.T) {
	d, dir := newDisk(t)
	const size = 16384
	rs := []run{{offset: 4096 - 50, data: []byte(strings.Repeat("x", 100))}}

	_, err := d.CreateFromExtents(context.Background(), entryOf(size), stream(size, rs...))

	require.NoError(t, err)
	got, err := os.ReadFile(filepath.Join(dir, "d"))
	require.NoError(t, err)
	assert.Equal(t, logicalOf(size, rs...), got)
}

func TestReadExtents_YieldsTheDataAndTheDigestOfTheWholeDisk(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	const size = 2 << 20
	rs := []run{{offset: 7, data: []byte("abc")}, {offset: 1<<20 + 3, data: []byte("xyz")}}
	want := logicalOf(size, rs...)
	_, err := d.CreateFromExtents(ctx, entryOf(size), stream(size, rs...))
	require.NoError(t, err)

	rebuilt := make([]byte, size)
	sum, err := d.ReadExtents(ctx, filepath.Join(dir, "d"), func(off uint64, b []byte) error {
		copy(rebuilt[off:], b)
		return nil
	})

	require.NoError(t, err)
	assert.Equal(t, want, rebuilt)
	assert.Equal(t, sumOf(want), sum, "the digest covers the holes as zeros")
}

func TestReadExtents_AnEmitErrorEndsTheReadAndAnEndedContextToo(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	_, err := d.CreateFromExtents(ctx, entryOf(4), stream(4, run{0, []byte("abcd")}))
	require.NoError(t, err)
	path := filepath.Join(dir, "d")

	_, err = d.ReadExtents(ctx, path, func(uint64, []byte) error { return io.ErrClosedPipe })
	assert.ErrorIs(t, err, io.ErrClosedPipe)

	ended, cancel := context.WithCancel(ctx)
	cancel()
	_, err = d.ReadExtents(ended, path, func(uint64, []byte) error { return nil })
	assert.ErrorIs(t, err, context.Canceled)

	_, err = d.ReadExtents(ctx, path, func(uint64, []byte) error { return nil })
	assert.NoError(t, err, "control: an unended read succeeds")
}

func TestReadExtents_ADeadlineEndsTheHashingOfAHugeHole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "huge")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Truncate(path, 8<<30))
	d, _ := newDisk(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()

	_, err := d.ReadExtents(ctx, path, func(uint64, []byte) error { return nil })

	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

func TestNewDisk_RemovesTheTemporariesAnEndedProcessLeft(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	_, err = d.CreateFromExtents(context.Background(), entryOf(4), stream(4, run{0, []byte("abcd")}))
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, machine.TempDiskPrefix+"123"), []byte("half"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, machine.TempDiskPrefix+"456"), nil, 0o600))

	_, err = diskfile.NewDisk(dir, mkfsPath)

	require.NoError(t, err)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the disk is kept and both temporaries are gone")
	assert.Equal(t, "d", entries[0].Name())
	_, err = diskfile.NewDisk(filepath.Join(dir, "absent"), mkfsPath)
	assert.NoError(t, err, "control: a directory that is not there has none")
}
