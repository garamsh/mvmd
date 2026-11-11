// Package hostcapacity reads the node's capacity from the kernel as
// nodecapacity.Reader: the CPUs from sched_getaffinity, the memory from
// /proc/meminfo and the space from statfs on the state directory (ADR 0039).
package hostcapacity

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
)

const meminfoPath = "/proc/meminfo"

// Reader reads the capacity of the host and of the filesystem of one
// directory.
type Reader struct {
	stateDir string
}

// New returns a Reader whose space figures are those of the filesystem holding
// stateDir.
func New(stateDir string) *Reader { return &Reader{stateDir: stateDir} }

// Read implements nodecapacity.Reader.
func (r *Reader) Read() (nodecapacity.Capacity, error) {
	var set unix.CPUSet
	if err := unix.SchedGetaffinity(0, &set); err != nil {
		return nodecapacity.Capacity{}, fmt.Errorf("read the CPUs: %w", err)
	}
	f, err := os.Open(meminfoPath)
	if err != nil {
		return nodecapacity.Capacity{}, fmt.Errorf("read the memory: %w", err)
	}
	defer f.Close()
	mem, err := memTotal(f)
	if err != nil {
		return nodecapacity.Capacity{}, err
	}
	var fs unix.Statfs_t
	if err := unix.Statfs(r.stateDir, &fs); err != nil {
		return nodecapacity.Capacity{}, fmt.Errorf("read the space of %s: %w", r.stateDir, err)
	}
	unit := uint64(fs.Frsize)
	return nodecapacity.Capacity{
		CPUs:                uint32(set.Count()),
		MemoryBytes:         mem,
		StateTotalBytes:     fs.Blocks * unit,
		StateAvailableBytes: fs.Bavail * unit,
	}, nil
}

// memTotal returns MemTotal of a /proc/meminfo in bytes; the file reports kB.
func memTotal(r io.Reader) (uint64, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("read the memory: MemTotal line %q is not a size in kB", sc.Text())
		}
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("read the memory: %w", err)
		}
		return kb * 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read the memory: %w", err)
	}
	return 0, fmt.Errorf("read the memory: %s has no MemTotal", meminfoPath)
}
