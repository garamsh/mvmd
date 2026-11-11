package snapshotstore

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"syscall"
)

// blockBytes is the unit of st_blocks, whatever the file system's block size.
const blockBytes = 512

// occupied returns the disk space the regular files under dir occupy:
// allocated blocks, so a sparse file counts what it holds and not its length.
func occupied(dir string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("the allocation of %s is not readable", path)
		}
		total += uint64(st.Blocks) * blockBytes
		return nil
	})
	return total, err
}
