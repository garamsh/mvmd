package supervisor

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// cpuPeriod is the bandwidth period of cpu.max, in microseconds: the kernel's
// default.
const cpuPeriod = 100000

// limits are the cgroup limits of one VMM process (ADR 0015).
type limits struct {
	memoryMax uint64
	pidsMax   uint32
	cpuQuota  uint64
	cpuPeriod uint64
}

// limitsFor computes the limits from the Machine's guest and the Driver's
// overhead: memory.max is the guest's memory plus the memory overhead, pids.max
// the Driver's task count, and cpu.max the guest's vCPUs plus the CPU overhead.
// It refuses a request whose figures would limit nothing or everything.
func limitsFor(req *pb.StartRequest) (limits, error) {
	p := req.GetProcess()
	switch {
	case p == nil:
		return limits{}, errors.New("the request has no process")
	case req.GetGuestMemoryBytes() == 0:
		return limits{}, errors.New("the request states no guest memory")
	case req.GetGuestVcpus() == 0:
		return limits{}, errors.New("the request states no guest vCPUs")
	case p.GetOverheadTasks() == 0:
		return limits{}, errors.New("the process states no task count")
	case math.IsNaN(p.GetOverheadCpus()) || math.IsInf(p.GetOverheadCpus(), 0) || p.GetOverheadCpus() < 0:
		return limits{}, fmt.Errorf("the process states a CPU overhead of %v", p.GetOverheadCpus())
	case req.GetGuestMemoryBytes()+p.GetOverheadMemoryBytes() < req.GetGuestMemoryBytes():
		return limits{}, errors.New("the guest memory and the memory overhead overflow")
	}
	cpus := float64(req.GetGuestVcpus()) + p.GetOverheadCpus()
	return limits{
		memoryMax: req.GetGuestMemoryBytes() + p.GetOverheadMemoryBytes(),
		pidsMax:   p.GetOverheadTasks(),
		cpuQuota:  uint64(math.Round(cpus * cpuPeriod)),
		cpuPeriod: cpuPeriod,
	}, nil
}

// write sets the limits in the cgroup's files. A file that does not exist is a
// controller not enabled for the cgroup, which fails the write.
func (l limits) write(dir string) error {
	for _, f := range []struct{ name, value string }{
		{"memory.max", fmt.Sprint(l.memoryMax)},
		{"pids.max", fmt.Sprint(l.pidsMax)},
		{"cpu.max", fmt.Sprintf("%d %d", l.cpuQuota, l.cpuPeriod)},
	} {
		file, err := os.OpenFile(filepath.Join(dir, f.name), os.O_WRONLY|os.O_TRUNC, 0)
		if err != nil {
			return fmt.Errorf("open %s of the cgroup: %w", f.name, err)
		}
		_, err = file.WriteString(f.value)
		if cerr := file.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("set %s to %q: %w", f.name, f.value, err)
		}
	}
	return nil
}
