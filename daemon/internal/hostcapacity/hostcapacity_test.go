package hostcapacity_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/hostcapacity"
)

// allowedCPUs counts the CPUs in the Cpus_allowed_list of /proc/self/status,
// a reading of the same affinity that does not go through sched_getaffinity.
func allowedCPUs(t *testing.T) uint32 {
	t.Helper()
	raw, err := os.ReadFile("/proc/self/status")
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		list, ok := strings.CutPrefix(line, "Cpus_allowed_list:")
		if !ok {
			continue
		}
		var n uint32
		for _, part := range strings.Split(strings.TrimSpace(list), ",") {
			lo, hi, isRange := strings.Cut(part, "-")
			first, err := strconv.Atoi(lo)
			require.NoError(t, err)
			last := first
			if isRange {
				last, err = strconv.Atoi(hi)
				require.NoError(t, err)
			}
			n += uint32(last - first + 1)
		}
		return n
	}
	require.FailNow(t, "/proc/self/status has no Cpus_allowed_list")
	return 0
}

// memTotalBytes reads MemTotal of /proc/meminfo, which the file gives in kB.
func memTotalBytes(t *testing.T) uint64 {
	t.Helper()
	raw, err := os.ReadFile("/proc/meminfo")
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "MemTotal:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		require.Len(t, fields, 2)
		require.Equal(t, "kB", fields[1])
		kb, err := strconv.ParseUint(fields[0], 10, 64)
		require.NoError(t, err)
		return kb * 1024
	}
	require.FailNow(t, "/proc/meminfo has no MemTotal")
	return 0
}

// dfBytes reads the size and the available bytes of the filesystem of dir from
// df, a reading that does not go through statfs of this process.
func dfBytes(t *testing.T, dir string) (size, avail uint64) {
	t.Helper()
	out, err := exec.Command("df", "-B1", "--output=size,avail", dir).Output()
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	require.Len(t, lines, 2)
	fields := strings.Fields(lines[1])
	require.Len(t, fields, 2)
	size, err = strconv.ParseUint(fields[0], 10, 64)
	require.NoError(t, err)
	avail, err = strconv.ParseUint(fields[1], 10, 64)
	require.NoError(t, err)
	return size, avail
}

func TestRead_ReportsTheHostsCPUsAndMemory(t *testing.T) {
	got, err := hostcapacity.New(t.TempDir()).Read()
	require.NoError(t, err)
	assert.Equal(t, allowedCPUs(t), got.CPUs)
	// A figure in kB read as bytes would be 1024 times too small, and the
	// exact comparison fails for it.
	assert.Equal(t, memTotalBytes(t), got.MemoryBytes)
}

func TestRead_ReportsTheSpaceOfTheStateDirectorysFilesystem(t *testing.T) {
	dir := t.TempDir()
	got, err := hostcapacity.New(dir).Read()
	require.NoError(t, err)
	size, avail := dfBytes(t, dir)
	assert.Equal(t, size, got.StateTotalBytes)
	// Other writers move the available bytes between the two readings, so
	// the comparison is within 64 MiB.
	assert.InDelta(t, float64(avail), float64(got.StateAvailableBytes), 64<<20)
	assert.Less(t, got.StateAvailableBytes, got.StateTotalBytes, "control: some of the filesystem is in use")
}

func TestRead_ReadsTheAvailableBytesAtEachCall(t *testing.T) {
	dir := t.TempDir()
	reader := hostcapacity.New(dir)
	before, err := reader.Read()
	require.NoError(t, err)

	// Data that is not zero, so a filesystem that does not store zeroes
	// still uses the space.
	const written = 32 << 20
	f, err := os.Create(filepath.Join(dir, "used"))
	require.NoError(t, err)
	_, err = f.Write(bytes.Repeat([]byte{0xa5}, written))
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	_, usedAvail := dfBytes(t, dir)
	require.Less(t, usedAvail, before.StateAvailableBytes-written/2, "the setup used space on the filesystem")

	after, err := reader.Read()
	require.NoError(t, err)
	assert.Less(t, after.StateAvailableBytes, before.StateAvailableBytes-written/2)
}

func TestRead_FailsForAStateDirectoryThatDoesNotExist(t *testing.T) {
	_, err := hostcapacity.New(filepath.Join(t.TempDir(), "absent")).Read()
	assert.Error(t, err)
}
