package imagestore

import (
	"fmt"
	"os"
	"syscall"
)

// blockBytes is the unit of st_blocks, whatever the file system's block size.
const blockBytes = 512

// allocated returns the disk space the file at path occupies: its allocated
// blocks, so a sparse file counts what it holds and not its length.
func allocated(path string) (uint64, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("the allocation of %s is not readable", path)
	}
	return uint64(st.Blocks) * blockBytes, nil
}
