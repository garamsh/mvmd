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
// for every byte between them.
type diskHash struct {
	h   hash.Hash
	pos uint64
}

func newDiskHash() *diskHash { return &diskHash{h: sha256.New()} }

// run hashes the data at offset, with zeros from where the last run ended. It
// returns false for a run that begins before the last one ended.
func (d *diskHash) run(offset uint64, data []byte) bool {
	if offset < d.pos {
		return false
	}
	d.skipTo(offset)
	d.h.Write(data)
	d.pos += uint64(len(data))
	return true
}

// skipTo hashes zeros up to offset.
func (d *diskHash) skipTo(offset uint64) {
	for d.pos < offset {
		n := min(offset-d.pos, uint64(len(zeros)))
		d.h.Write(zeros[:n])
		d.pos += n
	}
}

func (d *diskHash) sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// fillFromExtents writes what the extents yield into the file, which is as long
// as the entry's size, and checks it is the disk the entry names.
func fillFromExtents(tmp *os.File, entry machine.DiskEntry, extents machine.DiskExtents) error {
	h := newDiskHash()
	for {
		offset, data, err := extents.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if offset+uint64(len(data)) > entry.SizeBytes || offset+uint64(len(data)) < offset {
			return fmt.Errorf("%w: an extent of %d bytes at %d is past the size %d", machine.ErrDiskStreamMismatch, len(data), offset, entry.SizeBytes)
		}
		if !h.run(offset, data) {
			return fmt.Errorf("%w: an extent at %d is not after the one before it", machine.ErrDiskStreamMismatch, offset)
		}
		write := func(off int64, b []byte) error {
			_, err := tmp.WriteAt(b, off)
			return err
		}
		if err := sparsecopy.WriteNonZero(write, data, int64(offset)); err != nil {
			return err
		}
	}
	h.skipTo(entry.SizeBytes)
	if got := h.sum(); got != entry.ContentsDigest {
		return fmt.Errorf("%w: the bytes received have SHA-256 %s, not %s", machine.ErrDiskStreamMismatch, got, entry.ContentsDigest)
	}
	return nil
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
	h := newDiskHash()
	err = sparsecopy.Runs(src, func(off int64, b []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		h.run(uint64(off), b)
		return emit(uint64(off), b)
	})
	if err != nil {
		return "", err
	}
	h.skipTo(uint64(info.Size()))
	return h.sum(), nil
}
