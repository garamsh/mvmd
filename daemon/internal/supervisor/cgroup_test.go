package supervisor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

func request(memory uint64, vcpus uint32, over uint64, tasks uint32, cpus float64) *pb.StartRequest {
	return &pb.StartRequest{
		GuestMemoryBytes: memory,
		GuestVcpus:       vcpus,
		Process:          &pb.Process{OverheadMemoryBytes: over, OverheadTasks: tasks, OverheadCpus: cpus},
	}
}

func TestLimitsFor_AddsTheDriversOverheadToTheGuest(t *testing.T) {
	got, err := limitsFor(request(1<<30, 2, 64<<20, 40, 0.5))
	require.NoError(t, err)
	assert.Equal(t, limits{memoryMax: 1<<30 + 64<<20, pidsMax: 40, cpuQuota: 250000, cpuPeriod: 100000}, got)

	// Control: another guest and overhead give other limits.
	other, err := limitsFor(request(2<<30, 1, 0, 8, 0.25))
	require.NoError(t, err)
	assert.Equal(t, limits{memoryMax: 2 << 30, pidsMax: 8, cpuQuota: 125000, cpuPeriod: 100000}, other)
}

func TestLimitsFor_RefusesALimitThatLimitsNothingOrEverything(t *testing.T) {
	for name, req := range map[string]*pb.StartRequest{
		"no guest memory": request(0, 2, 1, 40, 0.5),
		"no vCPUs":        request(1<<30, 0, 1, 40, 0.5),
		"no tasks":        request(1<<30, 2, 1, 0, 0.5),
		"negative cpus":   request(1<<30, 2, 1, 40, -3),
		"no process":      {GuestMemoryBytes: 1 << 30, GuestVcpus: 1},
	} {
		_, err := limitsFor(req)
		assert.Error(t, err, name)
	}
}

// The limits are written into the files of a cgroup whose controllers are
// enabled; a missing file is a controller that is not, and fails the write.
func TestLimitsWrite_FillsTheControllersFiles(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []string{"memory.max", "pids.max", "cpu.max"} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, f), []byte("max"), 0o644))
	}
	l := limits{memoryMax: 1000, pidsMax: 7, cpuQuota: 150000, cpuPeriod: 100000}
	require.NoError(t, l.write(dir))
	for f, want := range map[string]string{"memory.max": "1000", "pids.max": "7", "cpu.max": "150000 100000"} {
		got, err := os.ReadFile(filepath.Join(dir, f))
		require.NoError(t, err)
		assert.Equal(t, want, string(got), f)
	}

	missing := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(missing, "memory.max"), []byte("max"), 0o644))
	assert.Error(t, l.write(missing), "pids.max and cpu.max do not exist")
}
