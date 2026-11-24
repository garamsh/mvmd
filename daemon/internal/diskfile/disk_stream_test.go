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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

type run struct {
	offset uint64
	data   []byte
}

type runs []run

func (r *runs) Next() (uint64, []byte, error) {
	if len(*r) == 0 {
		return 0, nil, io.EOF
	}
	n := (*r)[0]
	*r = (*r)[1:]
	return n.offset, n.data, nil
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

func TestCreateFromExtents_HolesAndZeroBlocksAreZerosInTheDigest(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	const size = 3 << 20
	rs := []run{{offset: 5, data: []byte("head")}, {offset: 2<<20 + 17, data: []byte("tail")}}
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: size, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(logicalOf(size, rs...))}

	in := runs(append([]run{{offset: 1 << 20, data: make([]byte, 4096)}}, rs...))
	sortRuns(in)
	require.NoError(t, d.CreateFromExtents(ctx, entry, &in))

	got, err := os.ReadFile(filepath.Join(dir, "d"))
	require.NoError(t, err)
	assert.Equal(t, logicalOf(size, rs...), got)
	st, err := os.Stat(filepath.Join(dir, "d"))
	require.NoError(t, err)
	assert.Equal(t, int64(size), st.Size())
	have, err := d.Get(ctx, "d")
	require.NoError(t, err)
	assert.Equal(t, entry, have, "the entry records the digest the stream was checked against")
}

func sortRuns(r runs) {
	for i := range r {
		for j := i + 1; j < len(r); j++ {
			if r[j].offset < r[i].offset {
				r[i], r[j] = r[j], r[i]
			}
		}
	}
}

func TestCreateFromExtents_ABadStreamLeavesNoFileAndNoTemporary(t *testing.T) {
	good := []run{{offset: 0, data: []byte("0123456789abcdef")}}
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 16, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(good[0].data)}
	for name, bad := range map[string][]run{
		"a changed byte":       {{offset: 0, data: []byte("0123456789abcdeX")}},
		"a missing extent":     {{offset: 0, data: []byte("01234567")}},
		"past the size":        {{offset: 8, data: []byte("0123456789")}},
		"out of order":         {{offset: 8, data: []byte("89abcdef")}, {offset: 0, data: []byte("01234567")}},
		"overlapping":          {{offset: 0, data: []byte("0123456789")}, {offset: 8, data: []byte("89abcdef")}},
		"an offset that wraps": {{offset: ^uint64(0) - 2, data: []byte("0123456789abcdef")}},
	} {
		t.Run(name, func(t *testing.T) {
			d, dir := newDisk(t)
			in := runs(bad)

			err := d.CreateFromExtents(context.Background(), entry, &in)

			assert.ErrorIs(t, err, machine.ErrDiskStreamMismatch)
			entries, rerr := os.ReadDir(dir)
			require.NoError(t, rerr)
			assert.Empty(t, entries, "no disk and no temporary file")
			ok := runs(good)
			require.NoError(t, d.CreateFromExtents(context.Background(), entry, &ok), "control: the right stream is accepted")
		})
	}
}

func TestCreateFromExtents_ARepeatIsErrDiskExists(t *testing.T) {
	d, _ := newDisk(t)
	ctx := context.Background()
	data := []byte("abcd")
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 4, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(data)}
	first := runs{{0, data}}
	require.NoError(t, d.CreateFromExtents(ctx, entry, &first))

	second := runs{{0, data}}
	err := d.CreateFromExtents(ctx, entry, &second)

	assert.ErrorIs(t, err, machine.ErrDiskExists)
}

func TestCreateFromExtents_AnExtentAcrossABlockBoundaryIsKeptWhole(t *testing.T) {
	d, dir := newDisk(t)
	const size = 16384
	data := []byte(strings.Repeat("x", 100))
	rs := []run{{offset: 4096 - 50, data: data}}
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: size, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(logicalOf(size, rs...))}
	in := runs(rs)

	require.NoError(t, d.CreateFromExtents(context.Background(), entry, &in))

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
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: size, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(want)}
	in := runs(rs)
	require.NoError(t, d.CreateFromExtents(ctx, entry, &in))

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
	data := []byte("abcd")
	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 4, Filesystem: machine.FilesystemNone, ContentsDigest: sumOf(data)}
	in := runs{{0, data}}
	require.NoError(t, d.CreateFromExtents(ctx, entry, &in))
	path := filepath.Join(dir, "d")

	_, err := d.ReadExtents(ctx, path, func(uint64, []byte) error { return io.ErrClosedPipe })
	assert.ErrorIs(t, err, io.ErrClosedPipe)

	ended, cancel := context.WithCancel(ctx)
	cancel()
	_, err = d.ReadExtents(ended, path, func(uint64, []byte) error { return nil })
	assert.ErrorIs(t, err, context.Canceled)

	_, err = d.ReadExtents(ctx, path, func(uint64, []byte) error { return nil })
	assert.NoError(t, err, "control: an unended read succeeds")
}
