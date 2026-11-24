package diskfile

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

// zeros is what the digest of a disk takes for a hole.
var zeros [1 << 20]byte

// diskHash hashes a disk as a reader sees it: the runs the file holds, and zeros
// for every byte between them. The zeros of a large disk take long to hash, so
// each step of them looks at the context.
type diskHash struct {
	ctx context.Context
	h   hash.Hash
	pos uint64
}

func newDiskHash(ctx context.Context) *diskHash { return &diskHash{ctx: ctx, h: sha256.New()} }

// run hashes the data at offset, with zeros from where the last run ended. It
// returns false for a run that begins before the last one ended, and the error
// of the context.
func (d *diskHash) run(offset uint64, data []byte) (bool, error) {
	if offset < d.pos {
		return false, nil
	}
	if err := d.skipTo(offset); err != nil {
		return false, err
	}
	d.h.Write(data)
	d.pos += uint64(len(data))
	return true, nil
}

// skipTo hashes zeros up to offset.
func (d *diskHash) skipTo(offset uint64) error {
	for d.pos < offset {
		if err := d.ctx.Err(); err != nil {
			return err
		}
		n := min(offset-d.pos, uint64(len(zeros)))
		d.h.Write(zeros[:n])
		d.pos += n
	}
	return nil
}

func (d *diskHash) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// fillFromExtents writes what the extents yield into the file, which is as long
// as the entry's size, and checks it is the disk the extents' digest names. It
// returns the digest.
func fillFromExtents(ctx context.Context, tmp *os.File, entry machine.DiskEntry, extents machine.DiskExtents) (string, error) {
	h := newDiskHash(ctx)
	write := func(off int64, b []byte) error {
		_, err := tmp.WriteAt(b, off)
		return err
	}
	for {
		offset, data, err := extents.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
		if offset+uint64(len(data)) > entry.SizeBytes || offset+uint64(len(data)) < offset {
			return "", fmt.Errorf("%w: an extent of %d bytes at %d is past the size %d", machine.ErrDiskStreamMismatch, len(data), offset, entry.SizeBytes)
		}
		// The order is what keeps the file the bytes the hash saw: an extent
		// that overlapped the one before would be written over it and hashed
		// after it.
		ok, err := h.run(offset, data)
		if err != nil {
			return "", err
		}
		if !ok {
			return "", fmt.Errorf("%w: an extent at %d is not after the one before it", machine.ErrDiskStreamMismatch, offset)
		}
		if err := sparsecopy.WriteNonZero(write, data, int64(offset)); err != nil {
			return "", err
		}
	}
	if err := h.skipTo(entry.SizeBytes); err != nil {
		return "", err
	}
	got := h.sum()
	if want := extents.Digest(); got != want {
		return "", fmt.Errorf("%w: the bytes received have SHA-256 %s, not %s", machine.ErrDiskStreamMismatch, got, want)
	}
	return got, nil
}

// ReadExtents implements machine.Disk.
func (d *Disk) ReadExtents(ctx context.Context, path string, emit func(uint64, []byte) error) (string, error) {
	src, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", err
	}
	h := newDiskHash(ctx)
	err = sparsecopy.Runs(src, func(off int64, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, err := h.run(uint64(off), b); err != nil {
			return err
		}
		return emit(uint64(off), b)
	})
	if err != nil {
		return "", err
	}
	if err := h.skipTo(uint64(info.Size())); err != nil {
		return "", err
	}
	return h.sum(), nil
}
