//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const (
	// The UID range startNodeWith configures as vmm_uid_range.
	vmmUIDFirst = 100000
	vmmUIDCount = 1000
	// The memory the Drivers add to the guest's for the VMM's cgroup, and the CPUs they add to the
	// guest's vCPUs.
	vmmMemoryOverhead = 128 << 20
	vmmCPUOverhead    = 1
	cpuPeriod         = 100000
	noCapabilities    = "0000000000000000"
)

// A real VMM process, found from the host through its cgroup, is confined as ADR 0015 states:
// its own user, no capabilities, no_new_privs and seccomp, its own mount namespace holding only
// what the ADR lists, the network namespace its Driver decides, and its own cgroup with the
// limits. Each reading is made again of processes that are not confined, and fails there.
func TestE2E_ARealVMMIsConfinedAsADR0015States(t *testing.T) {
	const id = "confined"
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "confinement")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	req := createRequest(id, pulled.Digest)
	n.runMachine(t, ctx, req)

	machineCgroup := "/" + filepath.Base(n.work) + "/" + id
	vmm := vmmProcessOf(t, "/sys/fs/cgroup"+machineCgroup)
	daemon := n.cmd.Process.Pid

	var runDir syscall.Stat_t
	require.NoError(t, syscall.Stat(filepath.Join(n.stateDir, "machines", id, "run"), &runDir), "the Machine's run directory")
	// Firecracker opens its tap by name, so ADR 0015 leaves it in the host's network namespace;
	// Cloud Hypervisor takes the tap by descriptor and runs in a namespace of its own.
	expected := confinement{
		uid:              runDir.Uid,
		daemonMnt:        namespaceOf(t, daemon, "mnt"),
		daemonNet:        namespaceOf(t, daemon, "net"),
		daemonCgroup:     cgroupOf(t, daemon),
		sameNet:          driver.driver == "firecracker",
		cgroup:           machineCgroup,
		memoryMax:        req.Resources.MemoryBytes + vmmMemoryOverhead,
		cpuMax:           fmt.Sprintf("%d %d", (uint64(req.Resources.CpuCount)+vmmCPUOverhead)*cpuPeriod, cpuPeriod),
		vcpus:            int(req.Resources.CpuCount),
		allowedRootPaths: rootPathsOf(n),
		supervisor:       parentOf(t, vmm),
	}
	require.NotZero(t, expected.uid, "the Machine's run directory belongs to the Machine's user and not to root")

	got := expected.observe(t, vmm)
	got.log(t, "the VMM")
	assert.Empty(t, expected.failures(got), "the VMM of a Machine on the %s Driver is confined as ADR 0015 states", driver.driver)

	// The controls: each reading made of a process that is not confined fails the same check, so
	// the check cannot pass on an unconfined process.
	daemonFacts := expected.observe(t, daemon)
	daemonFacts.log(t, "control, mvmd")
	wantFailing := []string{
		"uid and gid", "CapEff", "CapPrm", "CapBnd", "NoNewPrivs", "Seccomp",
		"mount namespace", "root contents", "cgroup", "memory.max", "pids.max", "cpu.max",
	}
	if _, reported := daemonFacts.status["Seccomp_filters"]; reported {
		wantFailing = append(wantFailing, "Seccomp filters")
	}
	if !expected.sameNet {
		wantFailing = append(wantFailing, "network namespace")
	}
	requireFailing(t, expected.failures(daemonFacts), wantFailing...)

	// mvmd holds no ambient capability and no supplementary group, so these two readings and,
	// where the VMM shares the host's network namespace, that one have a process made for them.
	other := expected.observe(t, spawn(t, &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWNET,
		AmbientCaps: []uintptr{unix.CAP_NET_RAW},
	}))
	other.log(t, "control, a process with an ambient capability in a network namespace of its own")
	wantFailing = []string{"CapAmb"}
	if expected.sameNet {
		wantFailing = append(wantFailing, "network namespace")
	}
	requireFailing(t, expected.failures(other), wantFailing...)

	grouped := expected.observe(t, spawn(t, &syscall.SysProcAttr{Credential: &syscall.Credential{Groups: []uint32{4242}}}))
	grouped.log(t, "control, a process with a supplementary group")
	requireFailing(t, expected.failures(grouped), "supplementary groups")

	// A process cannot be put in the VMM's Landlock domain from outside: nsenter joins its
	// namespaces and takes its credentials and not its Landlock domain, which only the process
	// itself and its children hold. What the domain denies is shown by the unit test
	// TestConfinement_TheVMMCannotOpenAHostFileOutsideItsGrant in daemon/internal/supervision and by
	// TestEnforceLandlock_TakesAwayWhatNoRuleGrants in daemon/internal/supervisor, which run a probe
	// under the enforcement the supervisor applies.
	t.Log("Landlock: not shown by an attempt here; no process outside the VMM's domain can attempt what the domain denies")
}

// confinement is what ADR 0015 states of a VMM, and what the readings of a process are held to.
type confinement struct {
	uid              uint32
	daemonMnt        string
	daemonNet        string
	daemonCgroup     string
	sameNet          bool
	cgroup           string
	memoryMax        uint64
	cpuMax           string
	vcpus            int
	allowedRootPaths []string
	supervisor       int
}

// observed is what is read of one process: its status, its namespaces, its cgroup and the
// limits and members of that, and the paths of its root that are not allowed.
type observed struct {
	pid     int
	status  map[string]string
	mnt     string
	net     string
	cgroup  string
	limits  map[string]string
	members []int
	strays  []string
}

// observe reads the process from the host's /proc. It reads the root of the process by walking it
// from /proc/<pid>/root and stops descending at a path that is not allowed, so that the root of
// the host is read to its first level and a VMM's root in full.
func (c confinement) observe(t *testing.T, pid int) observed {
	t.Helper()
	o := observed{
		pid: pid, status: statusOf(t, pid), mnt: namespaceOf(t, pid, "mnt"), net: namespaceOf(t, pid, "net"),
		cgroup: cgroupOf(t, pid), limits: map[string]string{},
	}
	dir := "/sys/fs/cgroup" + o.cgroup
	for _, name := range []string{"memory.max", "pids.max", "cpu.max"} {
		raw, _ := os.ReadFile(filepath.Join(dir, name))
		o.limits[name] = strings.TrimSpace(string(raw))
	}
	o.members = cgroupProcs(dir)
	o.strays = c.strays(pid, "/")
	return o
}

// strays lists the paths under the directory of the process's root that are not allowed.
func (c confinement) strays(pid int, dir string) []string {
	entries, err := os.ReadDir(filepath.Join(fmt.Sprintf("/proc/%d/root", pid), dir))
	if err != nil {
		return []string{dir + " (unreadable: " + err.Error() + ")"}
	}
	var strays []string
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case !c.allowed(path):
			strays = append(strays, path)
		case e.IsDir() && path != "/run":
			strays = append(strays, c.strays(pid, path)...)
		}
	}
	return strays
}

var diskPath = regexp.MustCompile(`^/disk(/[0-9]+)?$`)

// allowed reports whether the path is one the root of a VMM holds, or a directory on the way to
// one. What is under the run directory is the VMM's own.
func (c confinement) allowed(path string) bool {
	if diskPath.MatchString(path) {
		return true
	}
	for _, p := range c.allowedRootPaths {
		if path == p || strings.HasPrefix(p, path+"/") {
			return true
		}
	}
	return false
}

// rootPathsOf is what ADR 0015 lists the root as holding for the selected Driver: its executable,
// the boot files, the disks (matched by diskPath), the Driver's device nodes and the run
// directory, and on aarch64 the CPU files the Drivers read. No /proc and no /sys is among them
// on x86_64.
func rootPathsOf(n *node) []string {
	paths := []string{
		filepath.Join(n.work, "inputs", filepath.Base(os.Getenv(driver.binaryEnv))),
		"/boot/kernel", "/boot/initrd", "/run", "/dev/kvm",
	}
	switch driver.driver {
	case "firecracker":
		paths = append(paths, "/dev/net/tun")
	case "cloud-hypervisor":
		paths = append(paths, "/dev/urandom")
	}
	if runtime.GOARCH == "arm64" {
		paths = append(paths, "/sys/devices/system/cpu/cpu0/cache", "/sys/devices/system/cpu/cpu0/regs/identification/midr_el1")
	}
	return paths
}

// failures names each assertion the readings of a process do not meet, with what was read. A
// confined VMM has none.
func (c confinement) failures(o observed) map[string]string {
	f := map[string]string{}
	fail := func(name, format string, args ...any) { f[name] = fmt.Sprintf(format, args...) }

	uid := strconv.FormatUint(uint64(c.uid), 10)
	for _, ids := range []string{o.status["Uid"], o.status["Gid"]} {
		for _, id := range strings.Fields(ids) {
			if id != uid {
				fail("uid and gid", "Uid %q, Gid %q, the Machine's %s", o.status["Uid"], o.status["Gid"], uid)
			}
		}
	}
	if len(strings.Fields(o.status["Uid"])) != 4 || len(strings.Fields(o.status["Gid"])) != 4 {
		fail("uid and gid", "Uid %q, Gid %q hold fewer than four IDs", o.status["Uid"], o.status["Gid"])
	}
	if c.uid < vmmUIDFirst || c.uid >= vmmUIDFirst+vmmUIDCount {
		fail("uid and gid", "the Machine's %s is outside %d..%d", uid, vmmUIDFirst, vmmUIDFirst+vmmUIDCount-1)
	}
	if o.status["Groups"] != "" {
		fail("supplementary groups", "Groups %q", o.status["Groups"])
	}
	for _, set := range []string{"CapEff", "CapPrm", "CapBnd", "CapAmb"} {
		if o.status[set] != noCapabilities {
			fail(set, "%s", o.status[set])
		}
	}
	if o.status["NoNewPrivs"] != "1" {
		fail("NoNewPrivs", "%q", o.status["NoNewPrivs"])
	}
	if o.status["Seccomp"] != "2" {
		fail("Seccomp", "%q", o.status["Seccomp"])
	}
	if filters, reported := o.status["Seccomp_filters"]; reported {
		if count, err := strconv.Atoi(filters); err != nil || count < 2 {
			fail("Seccomp filters", "%q, the supervisor's baseline and the VMM's own make two", filters)
		}
	}
	if o.mnt == c.daemonMnt {
		fail("mount namespace", "%s is mvmd's", o.mnt)
	}
	if len(o.strays) > 0 {
		fail("root contents", "paths ADR 0015 does not list: %v", o.strays)
	}
	if (o.net == c.daemonNet) != c.sameNet {
		fail("network namespace", "%s, mvmd's is %s, and the Driver's decision is that it is the host's: %v", o.net, c.daemonNet, c.sameNet)
	}
	if o.cgroup != c.cgroup || o.cgroup == c.daemonCgroup || strings.HasPrefix(o.cgroup, c.daemonCgroup+"/") ||
		slices.Contains(o.members, c.supervisor) {
		fail("cgroup", "%s with members %v, wanted %s outside mvmd's %s and without the supervisor %d", o.cgroup, o.members, c.cgroup, c.daemonCgroup, c.supervisor)
	}
	if o.limits["memory.max"] != strconv.FormatUint(c.memoryMax, 10) {
		fail("memory.max", "%q, wanted the guest's memory plus the Driver's overhead, %d", o.limits["memory.max"], c.memoryMax)
	}
	// The Driver states the task count; what the ADR fixes is that it is a limit above the vCPUs
	// and above the threads the VMM runs.
	threads, _ := strconv.Atoi(o.status["Threads"])
	if tasks, err := strconv.Atoi(o.limits["pids.max"]); err != nil || tasks <= c.vcpus || tasks < threads {
		fail("pids.max", "%q, wanted a number above the %d vCPUs and the %d threads", o.limits["pids.max"], c.vcpus, threads)
	}
	if o.limits["cpu.max"] != c.cpuMax {
		fail("cpu.max", "%q, wanted the vCPUs plus the Driver's overhead, %q", o.limits["cpu.max"], c.cpuMax)
	}
	return f
}

// log writes every value the assertions read.
func (o observed) log(t *testing.T, who string) {
	t.Helper()
	for _, key := range []string{"Uid", "Gid", "Groups", "CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb", "NoNewPrivs", "Seccomp", "Seccomp_filters", "Threads"} {
		value, reported := o.status[key]
		if !reported {
			value = "(not reported by this kernel)"
		}
		t.Logf("%s (pid %d), %s: %q", who, o.pid, key, value)
	}
	t.Logf("%s (pid %d), mount namespace: %s, network namespace: %s", who, o.pid, o.mnt, o.net)
	t.Logf("%s (pid %d), cgroup: %s, members: %v", who, o.pid, o.cgroup, o.members)
	for _, name := range []string{"memory.max", "pids.max", "cpu.max"} {
		t.Logf("%s (pid %d), %s: %q", who, o.pid, name, o.limits[name])
	}
	t.Logf("%s (pid %d), paths of the root that ADR 0015 does not list: %v", who, o.pid, o.strays)
}

// requireFailing asserts that the failures include each named assertion.
func requireFailing(t *testing.T, failures map[string]string, names ...string) {
	t.Helper()
	for _, name := range names {
		_, failed := failures[name]
		require.True(t, failed, "control: the check %q passes on a process that is not confined; it failed only %v", name, failures)
	}
}

// spawn starts a sleeping process with the attributes, ended at the test's cleanup, and returns
// its host ID once it has executed.
func spawn(t *testing.T, attr *syscall.SysProcAttr) int {
	t.Helper()
	cmd := exec.Command("sleep", "600")
	cmd.SysProcAttr = attr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return cmd.Process.Pid
}

// vmmProcessOf returns the host ID of the VMM in the cgroup: the process whose parent is not
// another process of the cgroup, since a VMM may fork a helper and the supervisor, its parent, is
// outside.
func vmmProcessOf(t *testing.T, cgroupDir string) int {
	t.Helper()
	procs := cgroupProcs(cgroupDir)
	require.NotEmpty(t, procs, "the processes of %s", cgroupDir)
	var roots []int
	for _, pid := range procs {
		if !slices.Contains(procs, parentOf(t, pid)) {
			roots = append(roots, pid)
		}
	}
	require.Len(t, roots, 1, "the VMM among the processes %v of %s", procs, cgroupDir)
	return roots[0]
}

func cgroupProcs(dir string) []int {
	raw, _ := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	var pids []int
	for _, field := range strings.Fields(string(raw)) {
		if pid, err := strconv.Atoi(field); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

func statusOf(t *testing.T, pid int) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	require.NoError(t, err)
	status := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			status[key] = strings.TrimSpace(value)
		}
	}
	return status
}

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	parent, err := strconv.Atoi(statusOf(t, pid)["PPid"])
	require.NoError(t, err)
	return parent
}

func namespaceOf(t *testing.T, pid int, kind string) string {
	t.Helper()
	link, err := os.Readlink(fmt.Sprintf("/proc/%d/ns/%s", pid, kind))
	require.NoError(t, err)
	return link
}

// cgroupOf is the path of the process's cgroup under the cgroup v2 root.
func cgroupOf(t *testing.T, pid int) string {
	t.Helper()
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	require.NoError(t, err)
	for _, line := range strings.Split(string(raw), "\n") {
		if path, ok := strings.CutPrefix(line, "0::"); ok {
			return path
		}
	}
	t.Fatalf("pid %d is in no cgroup v2 path: %q", pid, raw)
	return ""
}
