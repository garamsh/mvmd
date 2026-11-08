package supervision_test

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

var ctx = context.Background()

// requireTree requires a process tree in the namespace that holds the VMM and
// a process daemonized out of the VMM's own tree, both of the Machine's user,
// beside the supervisor, which is root's.
func requireTree(t *testing.T, ns string) {
	t.Helper()
	got := members(ns)
	require.GreaterOrEqual(t, len(got), 3, "the supervisor, the VMM and the daemonized process are in the namespace: %v", got)
	users := 0
	for _, uid := range got {
		if uid == int(vmmUser.UID) {
			users++
		}
	}
	require.GreaterOrEqual(t, users, 2, "the VMM and the daemonized process run as the Machine's user: %v", got)
}

func TestExpiry_EndsTheProcessTree(t *testing.T) {
	n := newNode(t)
	expiry, ns := n.run(t, "m", 2*time.Second)
	requireTree(t, ns)

	// Control: the tree is alive until the expiry.
	time.Sleep(time.Second)
	assert.NotEmpty(t, members(ns), "the tree lives before the expiry")

	d := untilEnds(t, "expiry reached, from the expiry", ns, expiry)
	assert.GreaterOrEqual(t, d, time.Duration(0)-time.Millisecond, "nothing ended before the expiry")
	assert.Less(t, d, time.Second)
}

func TestSupervisorKilled_EndsTheProcessTreeAtOnce(t *testing.T) {
	n := newNode(t)
	_, ns := n.run(t, "m", time.Hour)
	requireTree(t, ns)
	sup := supervisorOf(t, "m")

	// Control: killing the daemonized process, which is not the supervisor and
	// not the VMM, leaves the supervisor and the rest of the tree running.
	sleepers := processes("sleeper", runDir(n.dir, "m"))
	require.Len(t, sleepers, 1)
	require.NoError(t, syscall.Kill(sleepers[0], syscall.SIGKILL))
	time.Sleep(100 * time.Millisecond)
	assert.NotEmpty(t, processes("--machine m "), "the supervisor outlives a process of its tree")

	killed := boottime(t)
	require.NoError(t, syscall.Kill(sup, syscall.SIGKILL))
	d := untilEnds(t, "supervisor SIGKILLed, from the kill", ns, killed)
	assert.Less(t, d, 500*time.Millisecond, "the tree ends at once")
}

func TestMvmdGone_TheSupervisorStillEndsTheTreeAtExpiry(t *testing.T) {
	dir := shortDir(t)
	expiry := boottime(t) + int64(2500*time.Millisecond)
	cmd := exec.Command(os.Args[0], "__mvmd__", dir, "m", strconv.FormatInt(expiry, 10))
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	buf := make([]byte, 16)
	k, _ := out.Read(buf)
	require.Equal(t, "started", strings.TrimSpace(string(buf[:k])))
	untilReady(t, runDir(dir, "m"))
	ns := namespaceOf(supervisorOf(t, "m"))
	requireTree(t, ns)

	// Control: with mvmd alive the supervisor is its child.
	assert.Equal(t, cmd.Process.Pid, parentOf(t, supervisorOf(t, "m")))

	require.NoError(t, cmd.Process.Kill())
	_ = cmd.Wait()
	time.Sleep(200 * time.Millisecond)
	assert.NotEmpty(t, members(ns), "the tree outlives mvmd's side")

	d := untilEnds(t, "mvmd's side gone", ns, expiry)
	assert.Less(t, d, time.Second)
}

func parentOf(t *testing.T, pid int) int {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	require.NoError(t, err)
	s := string(raw)
	f := strings.Fields(s[strings.LastIndex(s, ")")+2:])
	ppid, err := strconv.Atoi(f[1])
	require.NoError(t, err)
	return ppid
}

func TestRenewal_ExtendsAndShortens(t *testing.T) {
	t.Run("without a renewal the original expiry holds", func(t *testing.T) {
		n := newNode(t)
		expiry, ns := n.run(t, "m", 1500*time.Millisecond)
		d := untilEnds(t, "no renewal", ns, expiry)
		assert.Less(t, d, time.Second)
	})
	t.Run("extended", func(t *testing.T) {
		n := newNode(t)
		original, ns := n.run(t, "m", 1500*time.Millisecond)
		extended := original + int64(2*time.Second)
		require.NoError(t, n.a.SetExpiry(ctx, "m", fencing.BootTime(extended)))

		time.Sleep(time.Until(time.Now().Add(time.Duration(original-boottime(t)))) + 500*time.Millisecond)
		assert.NotEmpty(t, members(ns), "the tree outlives the original expiry")
		d := untilEnds(t, "extended", ns, extended)
		assert.Less(t, d, time.Second)
	})
	t.Run("shortened", func(t *testing.T) {
		n := newNode(t)
		_, ns := n.run(t, "m", time.Hour)
		shortened := boottime(t) + int64(time.Second)
		require.NoError(t, n.a.SetExpiry(ctx, "m", fencing.BootTime(shortened)))
		d := untilEnds(t, "shortened", ns, shortened)
		assert.Less(t, d, time.Second)
	})
}

func TestReattach_ANewAdapterFindsTheLiveSupervisors(t *testing.T) {
	first := newNode(t)
	exA, _ := first.run(t, "a", time.Hour)
	exB, nsB := first.run(t, "b", 2*time.Hour)

	// Control: an adapter over another directory finds none.
	other := newNode(t)
	live, err := other.a.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live)

	again, err := supervision.New(adapterConfig(first.dir))
	require.NoError(t, err)
	live, err = again.Live(ctx)
	require.NoError(t, err)
	held := map[machine.MachineID]fencing.BootTime{}
	for _, s := range live {
		held[s.Machine] = s.Expiry
	}
	assert.Equal(t, map[machine.MachineID]fencing.BootTime{"a": fencing.BootTime(exA), "b": fencing.BootTime(exB)}, held, "each supervisor and what it holds")

	// The new instance acts on what it found.
	require.NoError(t, again.SetExpiry(ctx, "b", fencing.BootTime(exB+1)))
	live, err = again.Live(ctx)
	require.NoError(t, err)
	for _, s := range live {
		if s.Machine == "b" {
			assert.Equal(t, fencing.BootTime(exB+1), s.Expiry)
		}
	}
	require.NoError(t, again.Terminate(ctx, "b"))
	assert.Empty(t, members(nsB), "Terminate returns once the namespace is empty")
	live, err = again.Live(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	assert.Equal(t, machine.MachineID("a"), live[0].Machine)

	require.NoError(t, again.Terminate(ctx, "b"), "terminating what does not exist succeeds")
}

// olderSupervisor answers as a release that reported less: no process state
// and no API version.
type olderSupervisor struct {
	pb.UnimplementedSupervisorServiceServer
	machine string
	expiry  int64
}

func (o *olderSupervisor) Inspect(context.Context, *pb.InspectRequest) (*pb.InspectResponse, error) {
	return &pb.InspectResponse{MachineId: o.machine, ExpiryBoottimeNs: o.expiry}, nil
}

func (o *olderSupervisor) SetExpiry(_ context.Context, r *pb.SetExpiryRequest) (*pb.SetExpiryResponse, error) {
	o.expiry = r.GetExpiryBoottimeNs()
	return &pb.SetExpiryResponse{ExpiryBoottimeNs: o.expiry}, nil
}

func TestOlderSupervisorAPI_IsStillReached(t *testing.T) {
	n := newNode(t)
	exNew, _ := n.run(t, "new", time.Hour)

	old := &olderSupervisor{machine: "old", expiry: 12345}
	require.NoError(t, os.MkdirAll(filepath.Join(n.dir, "old"), 0o700))
	l, err := net.Listen("unix", filepath.Join(n.dir, "old", "supervisor.sock"))
	require.NoError(t, err)
	g := grpc.NewServer()
	pb.RegisterSupervisorServiceServer(g, old)
	go g.Serve(l)
	t.Cleanup(g.Stop)

	live, err := n.a.Live(ctx)
	require.NoError(t, err)
	held := map[machine.MachineID]fencing.BootTime{}
	for _, s := range live {
		held[s.Machine] = s.Expiry
	}
	// Control: the current supervisor is listed beside it.
	assert.Equal(t, map[machine.MachineID]fencing.BootTime{"new": fencing.BootTime(exNew), "old": 12345}, held)

	require.NoError(t, n.a.SetExpiry(ctx, "old", 777))
	assert.Equal(t, int64(777), old.expiry)
}

func TestStart_RefusesAPassedExpiryAndNoExpiry(t *testing.T) {
	n := newNode(t)
	work := runDir(n.dir, "m")

	err := n.a.Start(ctx, "m", vmmProcess(work), vmmUser, fencing.BootTime(boottime(t)-int64(time.Second)))
	require.Error(t, err, "a passed expiry")
	assert.Contains(t, err.Error(), "passed")
	_, statErr := os.Stat(filepath.Join(work, "vmm.pid"))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no VMM process was started")
	assert.Empty(t, processes("--machine m "))

	// Without an expiry the supervisor itself refuses, run directly in a new
	// PID namespace as the adapter runs it.
	run := func(args ...string) (string, error) {
		cmd := exec.Command(supervisorBin, append([]string{"--socket", filepath.Join(n.dir, "raw.sock"), "--machine", "raw"}, args...)...)
		cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := run()
	require.Error(t, err)
	assert.Contains(t, out, "no expiry")

	// Control: the same command with a future expiry runs until killed.
	cmd := exec.Command(supervisorBin, "--socket", filepath.Join(n.dir, "ok.sock"), "--machine", "ok",
		"--expiry-boottime-ns="+strconv.FormatInt(boottime(t)+int64(time.Hour), 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	require.NoError(t, cmd.Start())
	time.Sleep(300 * time.Millisecond)
	assert.Nil(t, cmd.ProcessState, "the supervisor with an expiry runs")
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
}

// The supervisor never takes root as the Machine's user; the control is the
// nonzero user every other test starts with.
func TestStart_RefusesRootAsTheMachinesUser(t *testing.T) {
	n := newNode(t)
	for name, user := range map[string][2]uint32{"uid 0": {0, 65534}, "gid 0": {65534, 0}, "both 0": {0, 0}} {
		work := runDir(n.dir, "m")
		err := n.a.Start(ctx, "m", vmmProcess(work), machine.User{UID: user[0], GID: user[1]}, fencing.BootTime(boottime(t)+int64(time.Hour)))
		require.Error(t, err, name)
		assert.Contains(t, err.Error(), "root", name)
		_, statErr := os.Stat(filepath.Join(work, "vmm.pid"))
		assert.ErrorIs(t, statErr, os.ErrNotExist, "%s: no VMM process was started", name)
		assert.Empty(t, processes("--machine m "), "%s: the supervisor that refused is gone", name)
	}
	_, _ = n.run(t, "m", time.Hour)
}

// mvmd holds a pidfd to the VMM as well as to the supervisor: a new adapter
// that attaches finds both, and its Terminate returns only once the VMM has
// gone too.
func TestVMMPidfd_TerminateWaitsForTheVMM(t *testing.T) {
	n := newNode(t)
	_, ns := n.run(t, "m", time.Hour)
	work := runDir(n.dir, "m")
	require.Len(t, processes("vmm", work), 1, "control: the VMM runs before Terminate")
	require.NotEmpty(t, members(ns), "control: the namespace is occupied before Terminate")

	again, err := supervision.New(adapterConfig(n.dir))
	require.NoError(t, err)
	require.NoError(t, again.Terminate(ctx, "m"))
	assert.Empty(t, processes("vmm", work), "the VMM has ended when Terminate returns")
	assert.Empty(t, members(ns))
}

func TestTerminate_ReturnsOnceTheNamespaceIsEmpty(t *testing.T) {
	n := newNode(t)
	_, ns := n.run(t, "m", time.Hour)
	requireTree(t, ns)
	require.NoError(t, n.a.Terminate(ctx, "m"))
	assert.Empty(t, members(ns))
	live, err := n.a.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live)
}

func TestStart_RefusesAnIdentifierThatIsNotADirectoryName(t *testing.T) {
	n := newNode(t)
	for _, id := range []machine.MachineID{"", ".", "..", "a/b", "../x"} {
		err := n.a.Start(ctx, id, machine.ProcessDescription{Executable: "/bin/true"}, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
		assert.Error(t, err, "%q", id)
	}
	// Control: an ordinary identifier is accepted.
	_, _ = n.run(t, "ordinary", time.Hour)
}

// A supervisor starts in the configured cgroup, outside the one mvmd is in; the
// control is a supervisor started with no cgroup, which shares mvmd's.
func TestCgroup_TheSupervisorStartsOutsideMvmdsCgroup(t *testing.T) {
	own, err := os.ReadFile("/proc/self/cgroup")
	require.NoError(t, err)
	dir := filepath.Join("/sys/fs/cgroup", "mvmd-test-"+strconv.Itoa(os.Getpid()))
	require.NoError(t, os.Mkdir(dir, 0o755), "the test creates a cgroup, which needs a writable cgroup v2 mount")
	t.Cleanup(func() { _ = os.Remove(dir) })

	cgroupOf := func(pid int) string {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
		require.NoError(t, err)
		return strings.TrimSpace(string(raw))
	}
	plain := newNode(t)
	plain.run(t, "plain", time.Hour)
	assert.Equal(t, strings.TrimSpace(string(own)), cgroupOf(supervisorOf(t, "plain")), "control: without a cgroup the supervisor is in mvmd's")

	cfg := adapterConfig(shortDir(t))
	cfg.Cgroup = dir
	a, err := supervision.New(cfg)
	require.NoError(t, err)
	require.NoError(t, a.Start(ctx, "placed", vmmProcess(runDir(cfg.Dir, "placed")), vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour))))
	t.Cleanup(func() { _ = a.Terminate(ctx, "placed") })
	got := cgroupOf(supervisorOf(t, "placed"))
	assert.Contains(t, got, "mvmd-test-")
	assert.NotEqual(t, strings.TrimSpace(string(own)), got)
}
