package supervision_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
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
	"github.com/garamsh/mvmd/daemon/internal/statedir"
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

// vmmOf returns the host id of the probe running for the run directory: the one
// in its Machine's cgroup, since every probe names its directory /run.
func vmmOf(t *testing.T, run string) int {
	t.Helper()
	id := filepath.Base(filepath.Dir(run))
	pids := inCgroup(id, "probe", " report ")
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
		// Root starts with an empty inheritable and ambient set, so those two
		// have no control here; the other three do.
		if set != "CapInh" && set != "CapAmb" {
			assert.NotEqual(t, noCaps, control[set], "control: the same probe, run as root without the supervisor, has a %s", set)
		}
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
	run := n.started(t, "m", vmmUser, append(tries, "write:"+filepath.Join(machine.InRunDir, "mine")), func(p *machine.ProcessDescription) {
		p.ReadFiles = []string{granted.Name()}
	})
	rep := reportOf(t, run)
	ctl := outside(t, vmmUser.UID, tries...).report

	assert.Equal(t, "ENOENT", rep["open:"+secret.Name()], "the host file is not in the VMM's root")
	assert.Equal(t, "ok", ctl["open:"+secret.Name()], "control: the same user opens it without the supervisor")
	assert.Equal(t, "ok", rep["open:"+granted.Name()], "a file the Driver names is readable")
	assert.Equal(t, "EACCES", rep["opendir:/"], "the root exists in the VMM's mount namespace and no Landlock rule grants it")
	assert.Equal(t, "ok", ctl["opendir:/"], "control: the same user lists the root without the supervisor")
	assert.Equal(t, "ok", rep["write:"+filepath.Join(machine.InRunDir, "mine")], "the run directory is writable")
	assert.FileExists(t, filepath.Join(run, "mine"), "and it is the Machine's run directory on the host")
}

func TestConfinement_TheVMMCannotReachAnotherMachinesRunDirectory(t *testing.T) {
	n := newNode(t)
	other := runDir(n.dir, "b")
	intruder := filepath.Join(other, "intruder")
	own := filepath.Join(machine.InRunDir, "mine")

	_ = n.started(t, "b", machine.User{UID: 65533, GID: 65533}, nil, nil)
	run := n.started(t, "a", vmmUser, []string{"write:" + intruder, "write:" + own}, nil)
	rep := reportOf(t, run)
	ctl := outside(t, vmmUser.UID, "write:"+intruder).report

	assert.Equal(t, "ENOENT", rep["write:"+intruder], "the other Machine's run directory is not in the VMM's root")
	assert.Equal(t, "ok", rep["write:"+own], "its own run directory is, at /run")
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
	noTap := n.started(t, "notap", vmmUser, []string{"ifaces:", "fd3:"}, nil)
	name, fd, none := reportOf(t, byName), reportOf(t, byFD), reportOf(t, noTap)

	assert.Equal(t, "lo", fd["ifaces:"], "a VMM that takes its tap by descriptor has a network namespace with only its loopback")
	assert.Equal(t, "lo", none["ifaces:"], "a VMM with no network device, as a converter, has one too")
	assert.Equal(t, "EBADF", none["fd3:"], "and is passed no descriptor")
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
	// A VMM that opens its tap by name shares the host's network namespace,
	// where the socket is, so what refuses it is the Landlock scope. One with
	// no network device has a namespace of its own, where there is no such
	// socket.
	rep := reportOf(t, n.started(t, "byname", vmmUser, []string{try}, func(p *machine.ProcessDescription) {
		p.Tap = "mvmdt-byname"
	}))
	isolated := reportOf(t, n.started(t, "notap", vmmUser, []string{try}, nil))
	ctl := outside(t, vmmUser.UID, try).report
	assert.Equal(t, "EPERM", rep[try], "the Landlock scope refuses a socket outside the domain")
	assert.Equal(t, "ok", ctl[try], "control: the same user connects without the supervisor")
	assert.Equal(t, "ECONNREFUSED", isolated[try], "the host's abstract socket is not in a VMM's own namespace")
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
			Executable: probeBin, Args: []string{"vmm", machine.InRunDir},
			OverheadMemoryBytes: 256 << 20, OverheadTasks: 256, OverheadCpus: 1,
		},
		Uid: 65534, Gid: 65534,
		GuestMemoryBytes: 256 << 20, GuestVcpus: 1,
		RunDir: r.run, RunDirInPath: machine.InRunDir, Cgroup: r.cgroup,
	}
}

// A failed step fails the start before the VMM runs. The VMM writes its pid
// into its run directory first thing, so a run directory without it is a VMM
// that did not run; the control is the request without the fault, which runs.
func TestStart_AFailedStepFailsTheStartBeforeTheVMMRuns(t *testing.T) {
	notExecutable := filepath.Join(shortDir(t), "vmm")
	require.NoError(t, os.WriteFile(notExecutable, []byte("#!/bin/true\n"), 0o644))
	kernel := filepath.Join(shortDir(t), "kernel")
	require.NoError(t, os.WriteFile(kernel, []byte("a kernel"), 0o644))
	kernelFile := func(digest string) []*pb.BlockDevice {
		return []*pb.BlockDevice{{Path: kernel, InPath: machine.InKernel, ReadOnly: true, Sha256: digest}}
	}
	rightDigest, wrongDigest := sha256Hex([]byte("a kernel")), sha256Hex([]byte("another kernel"))

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
		{"a boot file that does not exist", func(_ *raw, req *pb.StartRequest) {
			req.Process.BootFiles = []*pb.BlockDevice{{Path: "/nonexistent/disk", InPath: machine.InDisk(0)}}
		}, "build the root"},
		{"a pinned kernel that is not the pinned file", func(_ *raw, req *pb.StartRequest) {
			req.Process.BootFiles = kernelFile(wrongDigest)
		}, "has digest " + rightDigest},
		{"a pinned executable that is not the pinned file", func(_ *raw, req *pb.StartRequest) {
			req.Process.ExecutableSha256 = wrongDigest
		}, "has digest"},
		{"two files at one path in the root", func(_ *raw, req *pb.StartRequest) {
			req.Process.BootFiles = append(kernelFile(rightDigest), &pb.BlockDevice{Path: notExecutable, InPath: machine.InKernel, ReadOnly: true})
		}, "both given the path"},
		{"a path in the root that leaves it", func(_ *raw, req *pb.StartRequest) {
			req.Process.BootFiles = []*pb.BlockDevice{{Path: kernel, InPath: "/boot/../../kernel"}}
		}, "absolute, clean"},
		{"a boot file with no path in the root", func(_ *raw, req *pb.StartRequest) {
			req.Process.BootFiles = []*pb.BlockDevice{{Path: kernel}}
		}, "absolute, clean"},
		{"a cgroup whose controllers are not enabled", func(r *raw, req *pb.StartRequest) {
			bare := filepath.Join(vmmBareCgroup, "bare")
			require.NoError(t, os.Mkdir(bare, 0o755))
			t.Cleanup(func() { removeCgroup(t, bare) })
			req.Cgroup = bare
		}, "set the cgroup limits"},
		{"an executable the Machine's user cannot execute", func(_ *raw, req *pb.StartRequest) {
			req.Process.Executable = notExecutable
		}, "execve"},
		{"a task count the confining child cannot start in", func(_ *raw, req *pb.StartRequest) {
			req.Process.OverheadTasks = 1
		}, "before it reached execve"},
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

	t.Run("control: the request without a fault, with the right pins, starts the VMM", func(t *testing.T) {
		r := newRaw(t, "control", vmmCgroup)
		probe, err := os.ReadFile(probeBin)
		require.NoError(t, err)
		req := r.request()
		req.Process.BootFiles = kernelFile(rightDigest)
		req.Process.ExecutableSha256 = sha256Hex(probe)
		_, err = r.client.Start(ctx, req)
		require.NoError(t, err)
		untilReady(t, r.run)
		_, statErr := os.Stat(filepath.Join(r.run, "vmm.pid"))
		assert.NoError(t, statErr, "the VMM ran")
	})
}

// The VMM's output goes to a file in its run directory, not to the log of the
// supervisor, which mvmd quotes in its errors and a compromised VMM must not
// write. A symlink a former VMM left in the run directory is not followed.
func TestConfinement_TheVMMWritesItsOwnLogAndNotTheSupervisors(t *testing.T) {
	n := newNode(t)
	run := runDir(n.dir, "m")
	victim := filepath.Join(shortDir(t), "victim")
	require.NoError(t, os.WriteFile(victim, []byte("untouched"), 0o600))
	require.NoError(t, os.MkdirAll(run, 0o700))
	require.NoError(t, os.Symlink(victim, filepath.Join(run, "vmm.log")))

	_, _ = n.run(t, "m", time.Hour)

	// The supervisor copies the VMM's output to the log as it reads it.
	require.Eventually(t, func() bool {
		log, err := os.ReadFile(filepath.Join(run, "vmm.log"))
		return err == nil && strings.Contains(string(log), "vmm started")
	}, 5*time.Second, 10*time.Millisecond, "the VMM's output is in its own log")
	info, err := os.Lstat(filepath.Join(run, "vmm.log"))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the planted symlink was replaced, not followed")
	assert.Equal(t, uint32(vmmUser.UID), info.Sys().(*syscall.Stat_t).Uid, "the log is the Machine's user's")
	got, err := os.ReadFile(victim)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(got), "the file the symlink named was not written")

	sup, err := os.ReadFile(filepath.Join(n.dir, "m", "supervisor.log"))
	require.NoError(t, err)
	assert.NotContains(t, string(sup), "vmm started", "control: the supervisor's log is not the VMM's output")
}

// The VMM's standard input and output are the console's terminal: what a session
// sends reaches the VMM and what the VMM writes reaches the session and the log
// (the probe echoes its input). The socket is the supervisor's, in the state
// directory beside the run directory, so the VMM, whose root holds the run
// directory alone, has no path to it, and only its owner can connect (ADR 0020, ADR 0025).
func TestConfinement_TheConsoleSocketReachesTheVMMsStandardInputAndOutput(t *testing.T) {
	n := newNode(t)
	_, _ = n.run(t, "m", time.Hour)
	sock := statedir.ConsoleSocket(filepath.Join(n.dir, "m"))

	info, err := os.Lstat(sock)
	require.NoError(t, err)
	assert.Equal(t, os.ModeSocket|0o600, info.Mode(), "a socket only its owner can open")
	assert.Equal(t, uint32(0), info.Sys().(*syscall.Stat_t).Uid, "owned by the supervisor's user, not the VMM's")
	assert.NotEqual(t, runDir(n.dir, "m"), filepath.Dir(sock), "and outside the directory the VMM owns")

	c, err := net.Dial("unix", sock)
	require.NoError(t, err)
	defer c.Close()
	_, err = c.Write([]byte("ping\n"))
	require.NoError(t, err)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(5*time.Second)))
	got := make([]byte, 5)
	_, err = io.ReadFull(c, got)
	require.NoError(t, err)
	assert.Equal(t, "ping\n", string(got), "the VMM read the input and wrote it to the session")

	require.Eventually(t, func() bool {
		log, err := os.ReadFile(filepath.Join(runDir(n.dir, "m"), "vmm.log"))
		return err == nil && strings.Contains(string(log), "ping\n")
	}, 5*time.Second, 10*time.Millisecond, "the log holds what the session saw")
}

// A VMM that outlives its guest's reset is ended by the supervisor when the
// event pipe the description names carries the match (ADR 0026). The probe
// writes the match to descriptor 7 in two pieces; the control writes other
// bytes the same way and its VMM runs on.
func TestConfinement_TheSupervisorEndsAVMMThatWritesItsMatchToTheEventPipe(t *testing.T) {
	n := newNode(t)
	start := func(id, text string) string {
		run := runDir(n.dir, id)
		proc := describe(probeBin, "events", run, "7", text)
		proc.EndOn = machine.PipeTrigger{FD: 7, Match: []byte(`"event": "rebooting"`)}
		require.NoError(t, n.a.Start(ctx, machine.MachineID(id), proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour))))
		return run
	}
	stays := start("stays", `{"source": "vm", "event": "booted"}`)
	// The match is the probe's second piece, so the supervisor may end this VMM
	// before the probe marks that it wrote: the end is what is waited on below.
	start("ends", `{"source": "vm", "event": "rebooting"}`)

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(stays, "written"))
		return err == nil
	}, 10*time.Second, 10*time.Millisecond, "the control wrote its bytes to the pipe")

	// The supervisor logs why before it kills the VMM.
	endsLog := filepath.Join(n.dir, "ends", "supervisor.log")
	require.Eventually(t, func() bool {
		log, err := os.ReadFile(endsLog)
		return err == nil && strings.Contains(string(log), "ending the VMM")
	}, 10*time.Second, 10*time.Millisecond, "the supervisor records why")
	require.Eventually(t, func() bool { return len(inCgroup("ends", "events")) == 0 },
		10*time.Second, 10*time.Millisecond, "the VMM that wrote the match is ended")

	assert.Len(t, inCgroup("stays", "events"), 1, "control: a VMM that wrote other bytes to its pipe runs on")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Each file of the boot description is in the VMM's root at the path the
// description gives it, with its mode, and not at its host path, so a snapshot
// that records these paths names nothing of the host (ADR 0019). The control is
// the same probe, outside the supervisor, reading the host paths.
func TestConfinement_EachFileIsAtItsFixedPathAndNotAtItsHostPath(t *testing.T) {
	dir := shortDir(t)
	file := func(name, content string, uid, gid uint32) string {
		p := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(p, []byte(content), 0o644))
		require.NoError(t, os.Chown(p, int(uid), int(gid)))
		return p
	}
	kernel := file("kernel", "a kernel", 0, 0)
	image := file("image", "an image", 0, 0)
	disk := file("root.img", "a disk", vmmUser.UID, vmmUser.GID)

	n := newNode(t)
	hostRun := runDir(n.dir, "m")
	tries := []string{
		"sha256:/boot/kernel", "sha256:/disk/0", "sha256:/disk/1",
		"sha256:" + kernel, "sha256:" + image, "sha256:" + disk,
		"openw:/boot/kernel", "openw:/disk/0", "openw:/disk/1",
		"write:/run/f", "write:" + hostRun + "/g",
	}
	run := n.started(t, "m", vmmUser, tries, func(p *machine.ProcessDescription) {
		p.BootFiles = []machine.File{
			{Host: kernel, In: machine.InKernel, ReadOnly: true, SHA256: sha256Hex([]byte("a kernel"))},
			{Host: image, In: machine.InDisk(0), ReadOnly: true},
			{Host: disk, In: machine.InDisk(1)},
		}
	})
	rep := reportOf(t, run)
	// The Machine's state directory is the supervisor's own, 0700, so the
	// control's user cannot even reach the run directory under it. Open the
	// directory to traversal only for the control, so that what it shows is the
	// path, not the mode above it.
	require.NoError(t, os.Chmod(filepath.Join(n.dir, "m"), 0o711))
	ctl := outside(t, vmmUser.UID, "sha256:"+kernel, "sha256:"+image, "sha256:"+disk, "write:"+hostRun+"/g").report

	assert.Equal(t, sha256Hex([]byte("a kernel")), rep["sha256:/boot/kernel"], "the kernel is at its fixed path")
	assert.Equal(t, sha256Hex([]byte("an image")), rep["sha256:/disk/0"], "the image is the device at position 0")
	assert.Equal(t, sha256Hex([]byte("a disk")), rep["sha256:/disk/1"], "the root disk is the device at position 1")
	for _, host := range []string{kernel, image, disk} {
		assert.Equal(t, "ENOENT", rep["sha256:"+host], "%s is not at its host path in the root", host)
		assert.NotEqual(t, "ENOENT", ctl["sha256:"+host], "control: the same user reaches %s without the supervisor", host)
	}
	for _, ro := range []string{"openw:/boot/kernel", "openw:/disk/0"} {
		assert.Contains(t, []string{"EROFS", "EACCES"}, rep[ro], "%s is read-only", ro)
	}
	assert.Equal(t, "ok", rep["openw:/disk/1"], "control: a device not marked read-only can be written")
	assert.Equal(t, "ok", rep["write:/run/f"], "the run directory is at /run")
	assert.FileExists(t, filepath.Join(hostRun, "f"), "and it is the Machine's own")
	assert.Equal(t, "ENOENT", rep["write:"+hostRun+"/g"], "not at its host path")
	assert.Equal(t, "ok", ctl["write:"+hostRun+"/g"], "control: the same user writes there without the supervisor")
}
