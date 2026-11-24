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
	if err := dst.Truncate(info.Size()); err != nil {
		return err
	}
	return Runs(src, func(off int64, b []byte) error {
		_, err := dst.WriteAt(b, off)
		return err
	})
}

// Runs calls emit with each run of src's blocks that holds a non-zero byte, in
// ascending order: only the data extents src reports (SEEK_DATA and SEEK_HOLE)
// are read, and each block that is all zeros is left out. A run is at most
// chunk bytes and valid until emit returns. An error of emit ends it.
func Runs(src *os.File, emit func(off int64, b []byte) error) error {
	info, err := src.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
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
		if err := runsOf(src, buf, data, min(end, size), emit); err != nil {
			return err
		}
		off = end
	}
	return nil
}

// runsOf emits the runs of [from, to) of src.
func runsOf(src *os.File, buf []byte, from, to int64, emit func(int64, []byte) error) error {
	for off := from; off < to; {
		n, err := src.ReadAt(buf[:min(int64(len(buf)), to-off)], off)
		if err != nil && !(errors.Is(err, io.EOF) && n > 0) {
			return err
		}
		if err := WriteNonZero(emit, buf[:n], off); err != nil {
			return err
		}
		off += int64(n)
	}
	return nil
}

// WriteNonZero calls write with each run of b's blocks that holds a non-zero
// byte, at its place counting from off, leaving out every block that is all
// zeros.
func WriteNonZero(write func(off int64, b []byte) error, b []byte, off int64) error {
	run := -1
	for i := 0; i < len(b); i += block {
		if !isZero(b[i:min(i+block, len(b))]) {
			if run < 0 {
				run = i
			}
			continue
		}
		if run >= 0 {
			if err := write(off+int64(run), b[run:i]); err != nil {
				return err
			}
			run = -1
		}
	}
	if run >= 0 {
		return write(off+int64(run), b[run:])
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
