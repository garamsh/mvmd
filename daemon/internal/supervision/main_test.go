package supervision_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// The tests start real supervisors, which need CAP_SYS_ADMIN (the PID
// namespace and the confinement's namespaces), CAP_SYS_NICE (the real-time
// policy), CAP_IPC_LOCK (mlockall) and the capabilities ADR 0015 lists for the
// confinement, and they create cgroups. A test binary that is not root runs
// itself again under passwordless sudo, which the GitHub-hosted runner
// provides; where it can get neither, the run fails, and no test is skipped.
//
// The VMM is the probe (internal/supervisor/testdata/probe), a static
// executable, since the root a VMM runs in holds no libraries.

const (
	supervisorEnv = "MVMD_SUPERVISOR_BIN"
	probeEnv      = "MVMD_PROBE_BIN"
	cgroupEnv     = "MVMD_VMM_CGROUP"
)

var (
	supervisorBin, probeBin string
	// vmmCgroup is the parent of the VMMs' cgroups in these tests, with the
	// controllers enabled; vmmBareCgroup has none enabled.
	vmmCgroup, vmmBareCgroup string
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && os.Args[1] == "__mvmd__" {
		mvmd(os.Args[2:])
		return
	}
	if os.Geteuid() != 0 {
		os.Exit(reexecUnderSudo())
	}
	os.Exit(run(m))
}

func run(m *testing.M) int {
	supervisorBin, probeBin = os.Getenv(supervisorEnv), os.Getenv(probeEnv)
	if supervisorBin == "" || probeBin == "" {
		dir, err := buildBinaries()
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
		defer os.RemoveAll(dir)
		supervisorBin, probeBin = filepath.Join(dir, "mvmd-supervisor"), filepath.Join(dir, "probe")
	}
	vmmCgroup = filepath.Join("/sys/fs/cgroup", "mvmd-test-vmm-"+strconv.Itoa(os.Getpid()))
	vmmBareCgroup = vmmCgroup + "-bare"
	if err := os.Mkdir(vmmCgroup, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "create a cgroup, which needs a writable cgroup v2 mount:", err)
		return 1
	}
	defer os.Remove(vmmCgroup)
	if err := os.Mkdir(vmmBareCgroup, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.Remove(vmmBareCgroup)
	if err := os.WriteFile(filepath.Join(vmmCgroup, "cgroup.subtree_control"), []byte("+cpu +memory +pids"), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "enable the controllers the VMM cgroups need:", err)
		return 1
	}
	os.Setenv(supervisorEnv, supervisorBin)
	os.Setenv(probeEnv, probeBin)
	os.Setenv(cgroupEnv, vmmCgroup)
	describeHost()
	return m.Run()
}

// buildBinaries builds the supervisor and the probe, as the invoking user, into
// a directory every user can enter, since the control runs of the probe are not
// root's.
func buildBinaries() (string, error) {
	dir, err := os.MkdirTemp("", "mvmd-supervisor-bin")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		return "", err
	}
	for _, b := range []struct{ out, pkg, env string }{
		{"mvmd-supervisor", "../../cmd/mvmd-supervisor", "GOFLAGS="},
		{"probe", "../supervisor/testdata/probe", "CGO_ENABLED=0"},
	} {
		cmd := exec.Command("go", "build", "-o", filepath.Join(dir, b.out), b.pkg)
		cmd.Env = append(os.Environ(), b.env)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return "", fmt.Errorf("build %s: %w", b.out, err)
		}
	}
	return dir, nil
}

// reexecUnderSudo builds the binaries as the invoking user, then runs this
// test binary as root with the same arguments and returns its exit code.
func reexecUnderSudo() int {
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "the supervision tests need root or passwordless sudo for the capabilities of the supervisor and the confinement, and this host gives neither:", err)
		return 1
	}
	dir, err := buildBinaries()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	args := append([]string{"-n", "env",
		supervisorEnv + "=" + filepath.Join(dir, "mvmd-supervisor"),
		probeEnv + "=" + filepath.Join(dir, "probe"),
		os.Args[0]}, os.Args[1:]...)
	cmd := exec.Command("sudo", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// mvmd is the test binary acting as mvmd: it starts a supervisor through the
// adapter, reports it, and waits to be killed.
func mvmd(args []string) {
	dir, id, expiry := args[0], args[1], args[2]
	n, _ := strconv.ParseInt(expiry, 10, 64)
	a, err := supervision.New(supervision.Config{
		Binary: os.Getenv(supervisorEnv), Dir: dir, VMMCgroup: os.Getenv(cgroupEnv),
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	proc := vmmProcessAt(os.Getenv(probeEnv), runDir(dir, id))
	if err := a.Start(context.Background(), machine.MachineID(id), proc, vmmUser, fencing.BootTime(n)); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("started")
	time.Sleep(24 * time.Hour)
}

// vmmUser is the unprivileged user the tests run the VMM as.
var vmmUser = machine.User{UID: 65534, GID: 65534}

// adapterConfig places the adapter's supervisors and VMM cgroups for a test.
func adapterConfig(dir string) supervision.Config {
	return supervision.Config{Binary: supervisorBin, Dir: dir, VMMCgroup: vmmCgroup}
}

// runDir is where the adapter puts the run directory of a Machine.
func runDir(dir, id string) string { return filepath.Join(dir, id, "run") }

// describe is the description of the probe in the mode, running in the
// directory: a guest of one vCPU and 256 MiB, and an overhead of 256 MiB, 256
// tasks and one CPU.
func describe(probe, mode, run string, args ...string) machine.ProcessDescription {
	return machine.ProcessDescription{
		Executable: probe,
		Args:       append([]string{mode, run}, args...),
		Overhead:   machine.ProcessOverhead{MemoryBytes: 256 << 20, Tasks: 256, CPUs: 1},
		Guest:      machine.Resources{VCPUs: 1, MemoryBytes: 256 << 20},
		RunDir:     run,
	}
}

// vmmProcessAt is a VMM that forks a daemonizing child and runs on.
func vmmProcessAt(probe, run string) machine.ProcessDescription {
	return describe(probe, "vmm", run)
}

func vmmProcess(run string) machine.ProcessDescription { return vmmProcessAt(probeBin, run) }

// describeHost prints the kernel, the CPU and the Landlock ABI the results
// come from.
func describeHost() {
	var u unix.Utsname
	_ = unix.Uname(&u)
	cpu := "unknown"
	if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "model name") {
				if _, v, ok := strings.Cut(line, ":"); ok {
					cpu = strings.TrimSpace(v)
				}
				break
			}
		}
	}
	abi, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	landlock := strconv.Itoa(int(abi))
	if errno != 0 {
		landlock = "none (" + errno.Error() + ")"
	}
	fmt.Printf("host: kernel %s, cpu %s, landlock abi %s\n", unix.ByteSliceToString(u.Release[:]), cpu, landlock)
}

func boottime(t testing.TB) int64 {
	t.Helper()
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		t.Fatal(err)
	}
	return ts.Nano()
}

// processes returns the processes of the host whose command line contains
// every word.
func processes(words ...string) []int {
	entries, _ := os.ReadDir("/proc")
	var out []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		raw, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		line := bytes.ReplaceAll(raw, []byte{0}, []byte{' '})
		match := true
		for _, w := range words {
			if !bytes.Contains(line, []byte(w)) {
				match = false
			}
		}
		if match {
			out = append(out, pid)
		}
	}
	return out
}

// namespaceOf returns the PID namespace link of the process.
func namespaceOf(pid int) string {
	link, _ := os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid))
	return link
}

// members returns the processes of the host in the PID namespace, with the
// user each runs as.
func members(ns string) map[int]int {
	out := map[int]int{}
	entries, _ := os.ReadDir("/proc")
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if namespaceOf(pid) != ns {
			continue
		}
		uid := -1
		if st, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid)); err == nil {
			for _, line := range strings.Split(string(st), "\n") {
				if f := strings.Fields(line); len(f) > 1 && f[0] == "Uid:" {
					uid, _ = strconv.Atoi(f[1])
				}
			}
		}
		out[pid] = uid
	}
	return out
}

// supervisorOf returns the host process id of the Machine's supervisor.
func supervisorOf(t testing.TB, id string) int {
	t.Helper()
	pids := processes("mvmd-supervisor", "--machine "+id+" ")
	if len(pids) != 1 {
		t.Fatalf("want one supervisor of %q, found %v", id, pids)
	}
	return pids[0]
}

// untilEnds waits for the namespace to empty and returns the time from the
// expiry to the end of the process tree, which it logs as the measured δ.
func untilEnds(t testing.TB, label, ns string, expiry int64) time.Duration {
	t.Helper()
	for len(members(ns)) > 0 {
		if boottime(t) > expiry+int64(10*time.Second) {
			t.Fatalf("%s: the process tree outlived the expiry by 10s", label)
		}
		time.Sleep(200 * time.Microsecond)
	}
	d := time.Duration(boottime(t) - expiry)
	t.Logf("δ %s: %v until the process tree ended", label, d)
	return d
}

func untilReady(t testing.TB, dir string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready")); err == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the VMM tree was not set up")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// shortDir returns a directory whose path leaves room for a socket name, and
// that every user can enter, so that what keeps a user out of a Machine's
// directory is the directory's own mode.
func shortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sv")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// node is an adapter with its own state directory.
type node struct {
	dir string
	a   *supervision.Adapter
}

func newNode(t testing.TB) *node {
	t.Helper()
	dir := shortDir(t)
	a, err := supervision.New(adapterConfig(dir))
	if err != nil {
		t.Fatal(err)
	}
	n := &node{dir: dir, a: a}
	t.Cleanup(func() {
		live, _ := a.Live(context.Background())
		for _, s := range live {
			_ = a.Terminate(context.Background(), s.Machine)
		}
	})
	return n
}

// run starts a supervised VMM tree for the Machine, expiring after the
// duration, and waits until the tree is set up. It returns the expiry and the
// supervisor's namespace.
func (n *node) run(t testing.TB, id string, after time.Duration) (expiry int64, ns string) {
	t.Helper()
	expiry = boottime(t) + int64(after)
	if err := n.a.Start(context.Background(), machine.MachineID(id), vmmProcess(runDir(n.dir, id)), vmmUser, fencing.BootTime(expiry)); err != nil {
		t.Fatalf("start the supervisor of %q: %v", id, err)
	}
	untilReady(t, runDir(n.dir, id))
	ns = namespaceOf(supervisorOf(t, id))
	return expiry, ns
}
