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
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// The tests start real supervisors, which need CAP_SYS_ADMIN (the PID
// namespace), CAP_SYS_NICE (the real-time policy) and CAP_IPC_LOCK (mlockall).
// A test binary that is not root runs itself again under passwordless sudo,
// which the GitHub-hosted runner provides; where it can get neither, the run
// fails, and no test is skipped.

const (
	roleEnv       = "MVMD_TEST_ROLE"
	supervisorEnv = "MVMD_SUPERVISOR_BIN"
)

var supervisorBin string

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && strings.HasPrefix(os.Args[1], "__") {
		helper(os.Args[1:])
		return
	}
	if os.Geteuid() != 0 {
		os.Exit(reexecUnderSudo())
	}
	supervisorBin = os.Getenv(supervisorEnv)
	if supervisorBin == "" {
		dir, err := os.MkdirTemp("", "mvmd-supervisor-bin")
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		defer os.RemoveAll(dir)
		supervisorBin = buildSupervisor(dir)
	}
	os.Exit(m.Run())
}

func buildSupervisor(dir string) string {
	out := filepath.Join(dir, "mvmd-supervisor")
	cmd := exec.Command("go", "build", "-o", out, "../../cmd/mvmd-supervisor")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build mvmd-supervisor:", err)
		os.Exit(1)
	}
	return out
}

// reexecUnderSudo builds the supervisor as the invoking user, then runs this
// test binary as root with the same arguments and returns its exit code.
func reexecUnderSudo() int {
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "the supervision tests need root or passwordless sudo for CAP_SYS_ADMIN, CAP_SYS_NICE and CAP_IPC_LOCK, and this host gives neither:", err)
		return 1
	}
	dir, err := os.MkdirTemp("", "mvmd-supervisor-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(dir)
	bin := buildSupervisor(dir)
	args := append([]string{"-n", "env", supervisorEnv + "=" + bin, os.Args[0]}, os.Args[1:]...)
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

// helper is the test binary acting as a VMM, a daemonizing child or mvmd.
func helper(args []string) {
	switch args[0] {
	case "__vmm__":
		// A VMM that forks: it starts a child that daemonizes and drops to
		// another user, and then runs on.
		dir := args[1]
		os.WriteFile(filepath.Join(dir, "vmm.pid"), []byte(strconv.Itoa(os.Getpid())), 0o600)
		cmd := exec.Command(os.Args[0], "__daemonize__", dir)
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := cmd.Start(); err != nil {
			os.Exit(1)
		}
		cmd.Wait()
		time.Sleep(24 * time.Hour)
	case "__daemonize__":
		// Starts a process as another user, in its own session, and exits, so
		// that the process is reparented to init.
		dir := args[1]
		sleeper := exec.Command("/bin/sleep", "1000")
		sleeper.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Credential: &syscall.Credential{Uid: 65534, Gid: 65534}}
		if err := sleeper.Start(); err != nil {
			os.Exit(1)
		}
		os.WriteFile(filepath.Join(dir, "ready"), nil, 0o600)
	case "__mvmd__":
		// mvmd's stand-in: starts a supervisor through the adapter, reports it,
		// and waits to be killed.
		dir, bin, id, expiry := args[1], args[2], args[3], args[4]
		n, _ := strconv.ParseInt(expiry, 10, 64)
		a, err := supervision.New(supervision.Config{Binary: bin, Dir: dir})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		work := filepath.Join(dir, "work-"+id)
		os.MkdirAll(work, 0o700)
		if err := a.Start(context.Background(), machine.MachineID(id), vmmProcess(work), fencing.BootTime(n)); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("started")
		time.Sleep(24 * time.Hour)
	}
}

func vmmProcess(work string) machine.ProcessDescription {
	return machine.ProcessDescription{Executable: os.Args[0], Args: []string{"__vmm__", work}}
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

// shortDir returns a directory whose path leaves room for a socket name.
func shortDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sv")
	if err != nil {
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
	a, err := supervision.New(supervision.Config{Binary: supervisorBin, Dir: dir})
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
	work := filepath.Join(n.dir, "work-"+id)
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	expiry = boottime(t) + int64(after)
	if err := n.a.Start(context.Background(), machine.MachineID(id), vmmProcess(work), fencing.BootTime(expiry)); err != nil {
		t.Fatalf("start the supervisor of %q: %v", id, err)
	}
	untilReady(t, work)
	ns = namespaceOf(supervisorOf(t, id))
	return expiry, ns
}
