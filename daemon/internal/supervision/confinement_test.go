package supervision_test

import (
	"encoding/json"
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
	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// The confinement tests start the probe as the VMM, ask it what it can do, and
// read the same things from the host's side of /proc and the cgroup. Each
// refusal is read beside a control: the same probe run without the supervisor,
// as the same user, doing the thing, or a Machine given the grant.

// started starts a Machine whose VMM is the probe in report mode, adjusts the
// description first, and returns its run directory.
func (n *node) started(t *testing.T, id string, user machine.User, tries []string, adjust func(*machine.ProcessDescription)) string {
	t.Helper()
	run := runDir(n.dir, id)
	proc := describe(probeBin, "report", run, tries...)
	if adjust != nil {
		adjust(&proc)
	}
	err := n.a.Start(ctx, machine.MachineID(id), proc, user, fencing.BootTime(boottime(t)+int64(time.Hour)))
	require.NoError(t, err, "start the VMM of %q", id)
	return run
}

// reportOf waits for the probe's report in the run directory and returns it.
func reportOf(t *testing.T, run string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(run, "report.json"))
		if err == nil {
			var out map[string]string
			require.NoError(t, json.Unmarshal(raw, &out))
			return out
		}
		require.False(t, time.Now().After(deadline), "the probe wrote no report in %s: %v", run, err)
		time.Sleep(10 * time.Millisecond)
	}
}

// outside runs the probe without a supervisor, as the user, and returns its
// report, which is the control for what the confinement takes away. The
// probe's process is left running for its host-side status, and killed at the
// end of the test.
type outsideProbe struct {
	report map[string]string
	pid    int
}

func outside(t *testing.T, uid uint32, tries ...string) outsideProbe {
	t.Helper()
	dir, err := os.MkdirTemp("", "outside")
	require.NoError(t, err)
	require.NoError(t, os.Chmod(dir, 0o777))
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	cmd := exec.Command(probeBin, append([]string{"report", dir}, tries...)...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uid, Gid: uid, NoSetGroups: false}}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	return outsideProbe{report: reportOf(t, dir), pid: cmd.Process.Pid}
}

// statusOf reads /proc/<pid>/status as the host sees it.
func statusOf(t *testing.T, pid int) map[string]string {
	t.Helper()
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	require.NoError(t, err)
	out := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			out[k] = strings.TrimSpace(v)
		}
	}
	return out
}

// vmmOf returns the host id of the probe running in the run directory.
func vmmOf(t *testing.T, run string) int {
	t.Helper()
	pids := processes("probe", "report "+run)
	require.Len(t, pids, 1, "the VMM of %s", run)
	return pids[0]
}

const noCaps = "0000000000000000"

func TestConfinement_TheVMMHoldsNoCapabilityAndCannotGainOne(t *testing.T) {
	n := newNode(t)
	run := n.started(t, "m", vmmUser, nil, nil)
	reportOf(t, run)
	st := statusOf(t, vmmOf(t, run))
	control := statusOf(t, outside(t, 0).pid)

	for _, set := range []string{"CapInh", "CapPrm", "CapEff", "CapBnd", "CapAmb"} {
		assert.Equal(t, noCaps, st[set], "the VMM's %s", set)
		assert.NotEqual(t, noCaps, control[set], "control: the same probe, run as root without the supervisor, has a %s", set)
	}
	assert.Equal(t, "1", st["NoNewPrivs"], "the VMM cannot gain a privilege")
	assert.Equal(t, "0", control["NoNewPrivs"], "control")
	assert.Equal(t, "2", st["Seccomp"], "the seccomp baseline is installed as a filter")
	assert.Equal(t, "0", control["Seccomp"], "control")
}

func TestConfinement_TheVMMRunsAsTheMachinesUser(t *testing.T) {
	n := newNode(t)
	for id, uid := range map[string]uint32{"a": 65534, "b": 65533} {
		run := n.started(t, id, machine.User{UID: uid, GID: uid}, nil, nil)
		rep := reportOf(t, run)
		want := strconv.Itoa(int(uid))
		assert.Equal(t, want+" "+want+" "+want, rep["uids"], "Machine %s: real, effective and saved user", id)
		assert.Equal(t, want+" "+want+" "+want, rep["gids"], "Machine %s: real, effective and saved group", id)
		assert.Equal(t, "0", rep["groups"], "Machine %s: no supplementary groups", id)
		st := statusOf(t, vmmOf(t, run))
		assert.Equal(t, strings.Join([]string{want, want, want, want}, "\t"), st["Uid"], "Machine %s as the host reads it", id)
		assert.Equal(t, "", st["Groups"], "Machine %s as the host reads it", id)
	}
	control := outside(t, 0)
	assert.Equal(t, "0 0 0", control.report["uids"], "control: without the supervisor the probe runs as the user that started it")
}

func TestConfinement_TheVMMCannotOpenAHostFileOutsideItsGrant(t *testing.T) {
	secret, err := os.CreateTemp("", "secret")
	require.NoError(t, err)
	granted, err := os.CreateTemp("", "granted")
	require.NoError(t, err)
	for _, f := range []*os.File{secret, granted} {
		require.NoError(t, f.Chmod(0o644))
		f.Close()
		t.Cleanup(func() { _ = os.Remove(f.Name()) })
	}
	tries := []string{"open:" + secret.Name(), "open:" + granted.Name(), "opendir:/"}

	n := newNode(t)
	run := n.started(t, "m", vmmUser, append(tries, "write:"+filepath.Join(runDir(n.dir, "m"), "mine")), func(p *machine.ProcessDescription) {
		p.ReadFiles = []string{granted.Name()}
	})
	rep := reportOf(t, run)
	ctl := outside(t, vmmUser.UID, tries...).report

	assert.Equal(t, "ENOENT", rep["open:"+secret.Name()], "the host file is not in the VMM's root")
	assert.Equal(t, "ok", ctl["open:"+secret.Name()], "control: the same user opens it without the supervisor")
	assert.Equal(t, "ok", rep["open:"+granted.Name()], "a file the Driver names is readable")
	assert.Equal(t, "EACCES", rep["opendir:/"], "the root exists in the VMM's mount namespace and no Landlock rule grants it")
	assert.Equal(t, "ok", ctl["opendir:/"], "control: the same user lists the root without the supervisor")
	assert.Equal(t, "ok", rep["write:"+filepath.Join(runDir(n.dir, "m"), "mine")], "the run directory is writable")
}

func TestConfinement_TheVMMCannotReachAnotherMachinesRunDirectory(t *testing.T) {
	n := newNode(t)
	other := runDir(n.dir, "b")
	intruder := filepath.Join(other, "intruder")
	own := filepath.Join(runDir(n.dir, "a"), "mine")

	_ = n.started(t, "b", machine.User{UID: 65533, GID: 65533}, nil, nil)
	run := n.started(t, "a", vmmUser, []string{"write:" + intruder, "write:" + own}, nil)
	rep := reportOf(t, run)
	ctl := outside(t, vmmUser.UID, "write:"+intruder).report

	assert.Equal(t, "ENOENT", rep["write:"+intruder], "the other Machine's run directory is not in the VMM's root")
	assert.Equal(t, "ok", rep["write:"+own], "its own run directory is")
	assert.Equal(t, "EACCES", ctl["write:"+intruder], "control: the directory exists, and without the supervisor only its owner's mode keeps this user out")
	_, err := os.Stat(intruder)
	assert.ErrorIs(t, err, os.ErrNotExist, "nothing was written there")
	info, err := os.Stat(other)
	require.NoError(t, err)
	assert.Equal(t, uint32(65533), info.Sys().(*syscall.Stat_t).Uid, "control: the other run directory is its own Machine's")
}

// pidsResult starts a Machine whose probe starts up to tries processes under a
// pids limit of tasks, and returns what it recorded.
func (n *node) pidsResult(t *testing.T, id string, tasks uint32, tries int) (started int, stoppedBy string, run string) {
	t.Helper()
	run = runDir(n.dir, id)
	proc := describe(probeBin, "pids", run, strconv.Itoa(tries))
	proc.Overhead.Tasks = tasks
	require.NoError(t, n.a.Start(ctx, machine.MachineID(id), proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour))))
	deadline := time.Now().Add(20 * time.Second)
	for {
		raw, err := os.ReadFile(filepath.Join(run, "pids.json"))
		if err == nil {
			var out struct {
				Started   int    `json:"started"`
				StoppedBy string `json:"stopped_by"`
			}
			require.NoError(t, json.Unmarshal(raw, &out))
			return out.Started, out.StoppedBy, run
		}
		require.False(t, time.Now().After(deadline), "the probe recorded nothing: %v", err)
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConfinement_TheVMMCannotExceedItsPidsLimit(t *testing.T) {
	n := newNode(t)
	const tries = 40
	started, stoppedBy, _ := n.pidsResult(t, "limited", 48, tries)
	assert.Less(t, started, tries, "a limit of 48 tasks stops the probe before it has started %d processes", tries)
	assert.Greater(t, started, 0, "the limit leaves room for some")
	assert.Equal(t, "EAGAIN", stoppedBy, "the kernel refuses the fork")

	// Control: the same probe under a limit that is not reached starts all.
	started, stoppedBy, _ = n.pidsResult(t, "roomy", 4096, tries)
	assert.Equal(t, tries, started)
	assert.Equal(t, "none", stoppedBy)
}

func TestConfinement_TheVMMStartsInItsOwnCgroupWithTheLimits(t *testing.T) {
	n := newNode(t)
	run := n.started(t, "m", vmmUser, nil, nil)
	reportOf(t, run)
	vmm, sup := vmmOf(t, run), supervisorOf(t, "m")

	cgroupOf := func(pid int) string {
		raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
		require.NoError(t, err)
		return strings.TrimSpace(string(raw))
	}
	dir := filepath.Join(vmmCgroup, "m")
	assert.Equal(t, "0::"+strings.TrimPrefix(dir, "/sys/fs/cgroup"), cgroupOf(vmm), "the VMM is in the cgroup mvmd made for it")
	assert.NotEqual(t, cgroupOf(vmm), cgroupOf(sup), "control: the supervisor is not in it")

	read := func(f string) string {
		raw, err := os.ReadFile(filepath.Join(dir, f))
		require.NoError(t, err)
		return strings.TrimSpace(string(raw))
	}
	assert.Equal(t, strconv.Itoa(256<<20+256<<20), read("memory.max"), "the guest's memory plus the Driver's overhead")
	assert.Equal(t, "256", read("pids.max"), "the Driver's task count")
	assert.Equal(t, "200000 100000", read("cpu.max"), "the vCPUs plus the Driver's CPU overhead, as two CPUs per period")

	require.NoError(t, n.a.Terminate(ctx, "m"))
	_, err := os.Stat(dir)
	assert.ErrorIs(t, err, os.ErrNotExist, "the cgroup is removed once the VMM has ended")
}

// createTap makes a persistent tap owned by the user in the host's network
// namespace, as the Network adapter will, and removes it at the end.
func createTap(t *testing.T, name string, user machine.User) {
	t.Helper()
	const (
		tunSetIff     = 0x400454ca
		tunSetPersist = 0x400454cb
		tunSetOwner   = 0x400454cc
		tunSetGroup   = 0x400454ce
	)
	attach := func() int {
		fd, err := unix.Open("/dev/net/tun", unix.O_RDWR|unix.O_CLOEXEC, 0)
		require.NoError(t, err, "the host has /dev/net/tun")
		ifr, err := unix.NewIfreq(name)
		require.NoError(t, err)
		ifr.SetUint16(unix.IFF_TAP | unix.IFF_NO_PI | unix.IFF_VNET_HDR)
		require.NoError(t, unix.IoctlIfreq(fd, tunSetIff, ifr))
		return fd
	}
	fd := attach()
	require.NoError(t, unix.IoctlSetInt(fd, tunSetOwner, int(user.UID)))
	require.NoError(t, unix.IoctlSetInt(fd, tunSetGroup, int(user.GID)))
	require.NoError(t, unix.IoctlSetInt(fd, tunSetPersist, 1))
	unix.Close(fd)
	t.Cleanup(func() {
		fd := attach()
		_ = unix.IoctlSetInt(fd, tunSetPersist, 0)
		unix.Close(fd)
	})
}

func TestConfinement_TheNamespacesFollowTheDriversTap(t *testing.T) {
	tap := "mvmdt" + strconv.Itoa(os.Getpid()%100000)
	createTap(t, tap, vmmUser)
	n := newNode(t)

	byName := n.started(t, "byname", vmmUser, []string{"ifaces:", "fd3:", "open:/dev/net/tun"}, func(p *machine.ProcessDescription) {
		p.DeviceNodes = []string{"/dev/net/tun"}
		p.Tap = tap
	})
	byFD := n.started(t, "byfd", vmmUser, []string{"ifaces:", "fd3:", "open:/dev/net/tun"}, func(p *machine.ProcessDescription) {
		p.TapByDescriptor = true
		p.Tap = tap
	})
	name, fd := reportOf(t, byName), reportOf(t, byFD)

	assert.Equal(t, "lo", fd["ifaces:"], "a VMM that takes its tap by descriptor has a network namespace with only its loopback")
	assert.Contains(t, name["ifaces:"], tap, "control: a VMM that opens its tap by name shares the host's namespace, where the tap is")
	assert.Equal(t, "ok:"+tap, fd["fd3:"], "the descriptor 3 is the Machine's tap, opened in the host's namespace")
	assert.Equal(t, "EBADF", name["fd3:"], "control: a Driver that takes no descriptor is passed none")
	assert.Equal(t, "ok", name["open:/dev/net/tun"], "control: the device node the Driver names is there")
	assert.Equal(t, "ENOENT", fd["open:/dev/net/tun"], "a device node the Driver does not name is not")
}

func TestConfinement_OnlyTheDeviceNodesTheDriverNamesAreThere(t *testing.T) {
	n := newNode(t)
	run := n.started(t, "m", vmmUser, []string{"ioctl:/dev/zero", "open:/dev/null"}, func(p *machine.ProcessDescription) {
		p.DeviceNodes = []string{"/dev/zero"}
	})
	rep := reportOf(t, run)
	ctl := outside(t, vmmUser.UID, "open:/dev/null").report

	assert.Equal(t, "ok", rep["ioctl:/dev/zero"], "the named device is opened, and an ioctl reaches its driver")
	assert.Equal(t, "ENOENT", rep["open:/dev/null"], "a device the Driver does not name is not in the root")
	assert.Equal(t, "ok", ctl["open:/dev/null"], "control: the same user opens it without the supervisor")
}

func TestConfinement_TheBaselineAnswersTheNamespaceCalls(t *testing.T) {
	tries := []string{"unshare:", "setns:", "clone3:", "userns-clone:", "fork:"}
	n := newNode(t)
	rep := reportOf(t, n.started(t, "m", vmmUser, tries, nil))
	ctl := outside(t, 0, tries...).report

	assert.Equal(t, "EPERM", rep["unshare:"])
	assert.Equal(t, "ok", ctl["unshare:"], "control: unshare(0) succeeds without the baseline")
	assert.Equal(t, "EPERM", rep["setns:"])
	assert.Equal(t, "EBADF", ctl["setns:"], "control")
	assert.Equal(t, "ENOSYS", rep["clone3:"])
	assert.NotEqual(t, "ENOSYS", ctl["clone3:"], "control: the kernel has clone3")
	assert.Equal(t, "EPERM", rep["userns-clone:"])
	assert.Equal(t, "ok", ctl["userns-clone:"], "control: root creates a user namespace without the baseline")
	assert.Equal(t, "ok", rep["fork:"], "a clone with no namespace flag is allowed")
}

func TestConfinement_TheVMMReachesNoAbstractSocketOfTheHost(t *testing.T) {
	name := "mvmd-confine-" + strconv.Itoa(os.Getpid())
	l, err := net.Listen("unix", "@"+name)
	require.NoError(t, err)
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	try := "abstract:" + name

	n := newNode(t)
	// Neither Machine takes its tap by descriptor, so both share the host's
	// network namespace, where the socket is.
	rep := reportOf(t, n.started(t, "m", vmmUser, []string{try}, nil))
	ctl := outside(t, vmmUser.UID, try).report
	assert.Equal(t, "EPERM", rep[try], "the Landlock scope refuses a socket outside the domain")
	assert.Equal(t, "ok", ctl[try], "control: the same user connects without the supervisor")
}

// raw is a supervisor started directly, in a PID namespace as the adapter
// starts it, so that a request can be made that the adapter would not make.
type raw struct {
	client pb.SupervisorServiceClient
	run    string
	cgroup string
}

func newRaw(t *testing.T, name, cgroupParent string) *raw {
	t.Helper()
	dir := shortDir(t)
	run := filepath.Join(dir, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	require.NoError(t, os.Chown(run, 65534, 65534))
	cg := filepath.Join(cgroupParent, name)
	require.NoError(t, os.Mkdir(cg, 0o755))
	// Registered before the supervisor's kill, so that it runs after: a cgroup
	// is removed once the processes in it have gone.
	t.Cleanup(func() { removeCgroup(t, cg) })

	sock := filepath.Join(dir, "s")
	cmd := exec.Command(supervisorBin, "--socket", sock, "--machine", name,
		"--expiry-boottime-ns="+strconv.FormatInt(boottime(t)+int64(time.Hour), 10))
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	require.NoError(t, cmd.Start())
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(sock); err == nil {
			break
		}
		require.False(t, time.Now().After(deadline), "the supervisor did not listen")
		time.Sleep(5 * time.Millisecond)
	}
	conn, err := grpc.NewClient("unix:"+sock, grpc.WithTransportCredentials(insecure.NewCredentials()))
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })
	return &raw{client: pb.NewSupervisorServiceClient(conn), run: run, cgroup: cg}
}

// removeCgroup removes a cgroup, which the kernel lets go of a moment after its
// last process has exited.
func removeCgroup(t *testing.T, dir string) {
	t.Helper()
	var err error
	for i := 0; i < 80; i++ {
		if err = os.Remove(dir); err == nil || os.IsNotExist(err) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("remove the cgroup %s: %v", dir, err)
}

func (r *raw) request() *pb.StartRequest {
	return &pb.StartRequest{
		Process: &pb.Process{
			Executable: probeBin, Args: []string{"vmm", r.run},
			OverheadMemoryBytes: 256 << 20, OverheadTasks: 256, OverheadCpus: 1,
		},
		Uid: 65534, Gid: 65534,
		GuestMemoryBytes: 256 << 20, GuestVcpus: 1,
		RunDir: r.run, Cgroup: r.cgroup,
	}
}

// A failed step fails the start before the VMM runs. The VMM writes its pid
// into its run directory first thing, so a run directory without it is a VMM
// that did not run; the control is the request without the fault, which runs.
func TestStart_AFailedStepFailsTheStartBeforeTheVMMRuns(t *testing.T) {
	notExecutable := filepath.Join(shortDir(t), "vmm")
	require.NoError(t, os.WriteFile(notExecutable, []byte("#!/bin/true\n"), 0o644))

	for _, c := range []struct {
		name   string
		change func(r *raw, req *pb.StartRequest)
		fails  string
	}{
		{"a tap that does not exist", func(_ *raw, req *pb.StartRequest) {
			req.Process.TapByDescriptor, req.Tap = true, "nosuchtap0"
		}, "open the tap"},
		{"a host file that does not exist", func(_ *raw, req *pb.StartRequest) {
			req.Process.ReadFiles = []string{"/nonexistent/file"}
		}, "build the root"},
		{"a device node that does not exist", func(_ *raw, req *pb.StartRequest) {
			req.Process.DeviceNodes = []string{"/dev/nosuchdevice"}
		}, "build the root"},
		{"a cgroup whose controllers are not enabled", func(r *raw, req *pb.StartRequest) {
			bare := filepath.Join(vmmBareCgroup, "bare")
			require.NoError(t, os.Mkdir(bare, 0o755))
			t.Cleanup(func() { removeCgroup(t, bare) })
			req.Cgroup = bare
		}, "set the cgroup limits"},
		{"an executable the Machine's user cannot execute", func(_ *raw, req *pb.StartRequest) {
			req.Process.Executable = notExecutable
		}, "execve"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRaw(t, "fails", vmmCgroup)
			req := r.request()
			c.change(r, req)
			_, err := r.client.Start(ctx, req)
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.fails, "the step that failed")
			_, statErr := os.Stat(filepath.Join(r.run, "vmm.pid"))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "the VMM did not run")
		})
	}

	t.Run("control: the request without a fault starts the VMM", func(t *testing.T) {
		r := newRaw(t, "control", vmmCgroup)
		_, err := r.client.Start(ctx, r.request())
		require.NoError(t, err)
		untilReady(t, r.run)
		_, statErr := os.Stat(filepath.Join(r.run, "vmm.pid"))
		assert.NoError(t, statErr, "the VMM ran")
	})
}
