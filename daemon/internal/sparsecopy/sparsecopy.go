// Package sparsecopy copies a file and keeps the copy as sparse as the source.
// It imports nothing of mvmd, so the adapters that copy a disk file share it.
package sparsecopy

import (
	"errors"
	"io"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// block is the unit in which a copy tells zeros from data, and chunk is how
// much of a data extent it reads at a time.
const (
	block = 4096
	chunk = 1 << 20
)

// Copy copies src to dst, which must be new and empty, and keeps dst as
// sparse as src is: dst is cut to src's size first, and only the data extents
// src reports (SEEK_DATA and SEEK_HOLE) are read, with each block that is all
// zeros skipped. Where the file system reports no holes, it reports the whole
// file as one extent, and the skipping alone keeps the copy sparse. Where dst's
// file system cannot hold holes, the copy is whole and still correct.
func Copy(dst, src *os.File) error {
	info, err := src.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if err := dst.Truncate(size); err != nil {
		return err
	}
	buf := make([]byte, chunk)
	for off := int64(0); off < size; {
		data, err := src.Seek(off, unix.SEEK_DATA)
		if errors.Is(err, syscall.ENXIO) {
			break
		}
		if err != nil {
			return err
		}
		end, err := src.Seek(data, unix.SEEK_HOLE)
		if err != nil {
			return err
		}
		if err := copyExtent(dst, src, buf, data, min(end, size)); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// copyExtent copies [from, to) of src to the same place in dst, writing runs
// of blocks that hold a non-zero byte.
func copyExtent(dst, src *os.File, buf []byte, from, to int64) error {
	for off := from; off < to; {
		n, err := src.ReadAt(buf[:min(int64(len(buf)), to-off)], off)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return err
		}
		if err := writeNonZero(dst, buf[:n], off); err != nil {
			return err
		}
		off += int64(n)
	}
	return nil
}

// writeNonZero writes b at off, leaving out every block of it that is all
// zeros.
func writeNonZero(dst *os.File, b []byte, off int64) error {
	run := -1
	for i := 0; i < len(b); i += block {
		if !isZero(b[i:min(i+block, len(b))]) {
			if run < 0 {
				run = i
			}
			continue
		}
		if run >= 0 {
			if _, err := dst.WriteAt(b[run:i], off+int64(run)); err != nil {
				return err
			}
			run = -1
		}
	}
	if run >= 0 {
		_, err := dst.WriteAt(b[run:], off+int64(run))
		return err
	}
	return nil
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}
