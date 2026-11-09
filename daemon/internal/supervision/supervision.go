// Package supervision is the adapter behind fencing.Supervisors: it starts one
// mvmd-supervisor per Machine, passes it expiries, kills it through a pidfd,
// and finds the supervisors that outlived an mvmd through their sockets in the
// Machines' state directories (ADR 0003). It speaks the supervisor API over
// each supervisor's Unix socket.
package supervision

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/supervisor"
	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

const (
	// DefaultVMMCgroup is the parent of the VMMs' cgroups when Config names
	// none: a child of the cgroup v2 root, outside mvmd's own cgroup.
	DefaultVMMCgroup = "/sys/fs/cgroup/mvmd-vmm"

	rpcTimeout   = 5 * time.Second
	startTimeout = 10 * time.Second
	endTimeout   = 10 * time.Second
)

// Config places the supervisors.
type Config struct {
	// Binary is the absolute path of mvmd-supervisor.
	Binary string
	// Dir holds one state directory per Machine, named by its identifier; each
	// supervisor's socket is in its Machine's.
	Dir string
	// Cgroup, when set, is a cgroup v2 directory each supervisor is started
	// in, so that it is outside mvmd's cgroup and a service manager stopping
	// mvmd does not stop it (ADR 0003). Empty leaves it in mvmd's.
	Cgroup string
	// VMMCgroup is the cgroup v2 directory under which each VMM gets a cgroup
	// of its own, created before its supervisor starts and removed after the
	// VMM has ended (ADR 0015). It must be outside mvmd's cgroup. Empty means
	// DefaultVMMCgroup.
	VMMCgroup string
}

// Adapter is a fencing.Supervisors over mvmd-supervisor processes.
type Adapter struct {
	cfg Config
}

var _ fencing.Supervisors = (*Adapter)(nil)

// New returns an Adapter. The supervisors it starts need what ADR 0003 lists:
// CAP_SYS_ADMIN of mvmd for the PID namespace, and CAP_SYS_NICE and
// CAP_IPC_LOCK of the supervisor.
func New(cfg Config) (*Adapter, error) {
	switch {
	case !filepath.IsAbs(cfg.Binary):
		return nil, fmt.Errorf("supervisor binary %q is not an absolute path", cfg.Binary)
	case !filepath.IsAbs(cfg.Dir):
		return nil, fmt.Errorf("supervisor directory %q is not an absolute path", cfg.Dir)
	case cfg.Cgroup != "" && !filepath.IsAbs(cfg.Cgroup):
		return nil, fmt.Errorf("supervisor cgroup %q is not an absolute path", cfg.Cgroup)
	case cfg.VMMCgroup != "" && !filepath.IsAbs(cfg.VMMCgroup):
		return nil, fmt.Errorf("VMM cgroup %q is not an absolute path", cfg.VMMCgroup)
	}
	if err := supervisor.CheckHost(); err != nil {
		return nil, err
	}
	if cfg.VMMCgroup == "" {
		cfg.VMMCgroup = DefaultVMMCgroup
	}
	return &Adapter{cfg: cfg}, nil
}

// machineDir returns the Machine's state directory. The identifier is the
// client's, so it is checked to name one directory directly under Dir.
func (a *Adapter) machineDir(id machine.MachineID) (string, error) {
	s := string(id)
	if s == "" || s == "." || s == ".." || strings.ContainsAny(s, "/\x00") {
		return "", fmt.Errorf("Machine identifier %q is not a directory name", s)
	}
	return filepath.Join(a.cfg.Dir, s), nil
}

// RunDir implements fencing.Supervisors. It is the one place the run
// directory is named: the lifecycle takes it from here, and Start refuses a
// description that names another.
func (a *Adapter) RunDir(id machine.MachineID) (string, error) {
	dir, err := a.machineDir(id)
	if err != nil {
		return "", err
	}
	return statedir.Run(dir), nil
}

// Start implements fencing.Supervisors.
func (a *Adapter) Start(ctx context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User, expiry fencing.BootTime) error {
	dir, err := a.machineDir(id)
	if err != nil {
		return err
	}
	// The description's run directory only has to agree: the path is this
	// adapter's, and nothing is created or handed to the VMM's user for any
	// other.
	runDir := statedir.Run(dir)
	if process.RunDir != runDir {
		return fmt.Errorf("the run directory %q is not the Machine's %q", process.RunDir, runDir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create the state directory: %w", err)
	}
	sock := statedir.SupervisorSocket(dir)
	switch _, err := peerPID(sock); {
	case err == nil:
		return fmt.Errorf("Machine %q already has a supervisor", id)
	case !isAbsent(err):
		return fmt.Errorf("check for a supervisor of %q: %w", id, err)
	}
	_ = os.Remove(sock)

	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return fmt.Errorf("create the run directory: %w", err)
	}
	if err := os.Chown(runDir, int(user.UID), int(user.GID)); err != nil {
		return fmt.Errorf("give the run directory to the Machine's user: %w", err)
	}
	vmmCgroup, err := a.createVMMCgroup(id)
	if err != nil {
		return err
	}

	logFile, err := os.OpenFile(statedir.SupervisorLog(dir), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		a.removeVMMCgroup(id)
		return fmt.Errorf("open the supervisor log: %w", err)
	}
	defer logFile.Close()

	cmd := exec.Command(a.cfg.Binary,
		"--socket", sock,
		"--machine", string(id),
		fmt.Sprintf("--expiry-boottime-ns=%d", int64(expiry)))
	cmd.Stdout, cmd.Stderr = logFile, logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID, Setpgid: true}
	if a.cfg.Cgroup != "" {
		fd, err := os.Open(a.cfg.Cgroup)
		if err != nil {
			return fmt.Errorf("open the supervisor cgroup: %w", err)
		}
		defer fd.Close()
		cmd.SysProcAttr.UseCgroupFD, cmd.SysProcAttr.CgroupFD = true, int(fd.Fd())
	}
	if err := cmd.Start(); err != nil {
		a.removeVMMCgroup(id)
		return fmt.Errorf("start the supervisor: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()

	deadline := time.After(startTimeout)
	for {
		if _, err := peerPID(sock); err == nil {
			break
		}
		select {
		case <-exited:
			a.removeVMMCgroup(id)
			return fmt.Errorf("the supervisor ended before it was ready: %s", logTail(dir))
		case <-deadline:
			_ = cmd.Process.Kill()
			a.removeVMMCgroup(id)
			return fmt.Errorf("the supervisor was not ready within %v: %s", startTimeout, logTail(dir))
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			a.removeVMMCgroup(id)
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}

	err = withClient(ctx, sock, func(ctx context.Context, c pb.SupervisorServiceClient) error {
		_, err := c.Start(ctx, &pb.StartRequest{
			Process:          toProcess(process),
			Uid:              user.UID,
			Gid:              user.GID,
			Tap:              process.Tap,
			GuestMemoryBytes: process.Guest.MemoryBytes,
			GuestVcpus:       process.Guest.VCPUs,
			RunDir:           runDir,
			RunDirInPath:     machine.InRunDir,
			Cgroup:           vmmCgroup,
		})
		return err
	})
	if err == nil {
		// The pidfd to the VMM is taken at the start: a started process that
		// cannot be held is not a start.
		var h *attached
		if h, err = attach(ctx, sock); err == nil {
			if h.vmmFD < 0 {
				err = errors.New("the supervisor reported no VMM process after its start")
			}
			h.close()
		}
	}
	if err != nil {
		if termErr := a.Terminate(context.WithoutCancel(ctx), id); termErr != nil {
			err = errors.Join(err, termErr)
		}
		return fmt.Errorf("start the process under its supervisor: %w", err)
	}
	return nil
}

// SetExpiry implements fencing.Supervisors.
func (a *Adapter) SetExpiry(ctx context.Context, id machine.MachineID, expiry fencing.BootTime) error {
	dir, err := a.machineDir(id)
	if err != nil {
		return err
	}
	return withClient(ctx, statedir.SupervisorSocket(dir), func(ctx context.Context, c pb.SupervisorServiceClient) error {
		_, err := c.SetExpiry(ctx, &pb.SetExpiryRequest{ExpiryBoottimeNs: int64(expiry)})
		return err
	})
}

// Terminate implements fencing.Supervisors. It kills the supervisor through a
// pidfd taken from its socket and returns once the supervisor, its VMM and
// every process of its namespace have ended.
func (a *Adapter) Terminate(ctx context.Context, id machine.MachineID) error {
	dir, err := a.machineDir(id)
	if err != nil {
		return err
	}
	sock := statedir.SupervisorSocket(dir)
	h, err := attach(ctx, sock)
	if isAbsent(err) {
		_ = os.Remove(sock)
		a.removeVMMCgroup(id)
		return nil
	}
	if err != nil {
		return fmt.Errorf("reach the supervisor of %q: %w", id, err)
	}
	defer h.close()
	if err := unix.PidfdSendSignal(h.supFD, unix.SIGKILL, nil, 0); err != nil && !errors.Is(err, unix.ESRCH) {
		return fmt.Errorf("kill the supervisor of %q: %w", id, err)
	}
	if err := waitReadable(h.supFD, endTimeout); err != nil {
		return fmt.Errorf("wait for the supervisor of %q to end: %w", id, err)
	}
	if h.vmmFD >= 0 {
		if err := waitReadable(h.vmmFD, endTimeout); err != nil {
			return fmt.Errorf("wait for the VMM of %q to end: %w", id, err)
		}
	}
	if err := waitNamespaceEmpty(h.ns, endTimeout); err != nil {
		return fmt.Errorf("wait for the namespace of %q to empty: %w", id, err)
	}
	_ = os.Remove(sock)
	a.removeVMMCgroup(id)
	return nil
}

// Live implements fencing.Supervisors. A socket nobody listens on belongs to a
// supervisor that has ended and is removed. A supervisor that listens and does
// not answer is listed with the earliest expiry, so that a caller that wants
// it to hold a given one passes it again.
func (a *Adapter) Live(ctx context.Context) ([]fencing.Supervisor, error) {
	entries, err := os.ReadDir(a.cfg.Dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list the state directories: %w", err)
	}
	var out []fencing.Supervisor
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sock := statedir.SupervisorSocket(filepath.Join(a.cfg.Dir, e.Name()))
		if _, err := peerPID(sock); err != nil {
			if isAbsent(err) {
				_ = os.Remove(sock)
				a.removeVMMCgroup(machine.MachineID(e.Name()))
				continue
			}
			return nil, fmt.Errorf("reach the supervisor in %s: %w", e.Name(), err)
		}
		sv := fencing.Supervisor{Machine: machine.MachineID(e.Name())}
		_ = withClient(ctx, sock, func(ctx context.Context, c pb.SupervisorServiceClient) error {
			resp, err := c.Inspect(ctx, &pb.InspectRequest{})
			if err != nil {
				return err
			}
			if resp.GetMachineId() != "" {
				sv.Machine = machine.MachineID(resp.GetMachineId())
			}
			sv.Expiry = fencing.BootTime(resp.GetExpiryBoottimeNs())
			return nil
		})
		out = append(out, sv)
	}
	return out, nil
}

// isAbsent reports that no process listens on the socket.
func isAbsent(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist)
}

// peerPID connects to the socket and returns the process id of its listener as
// the kernel reports it to this process.
func peerPID(sock string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), rpcTimeout)
	defer cancel()
	conn, err := unixsock.DialContext(ctx, sock)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	raw, err := conn.(*net.UnixConn).SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *unix.Ucred
	var credErr error
	if err := raw.Control(func(fd uintptr) {
		cred, credErr = unix.GetsockoptUcred(int(fd), unix.SOL_SOCKET, unix.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if credErr != nil {
		return 0, credErr
	}
	return int(cred.Pid), nil
}

// withClient runs f against the supervisor on the socket, bounded by
// rpcTimeout.
func withClient(ctx context.Context, sock string, f func(context.Context, pb.SupervisorServiceClient) error) error {
	conn, err := grpc.NewClient("passthrough:///supervisor",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dial context.Context, _ string) (net.Conn, error) {
			return unixsock.DialContext(dial, sock)
		}))
	if err != nil {
		return err
	}
	defer conn.Close()
	ctx, cancel := context.WithTimeout(ctx, rpcTimeout)
	defer cancel()
	return f(ctx, pb.NewSupervisorServiceClient(conn))
}

func toProcess(p machine.ProcessDescription) *pb.Process {
	out := &pb.Process{
		Executable:          p.Executable,
		Args:                p.Args,
		ReadFiles:           p.ReadFiles,
		DeviceNodes:         p.DeviceNodes,
		TapByDescriptor:     p.TapByDescriptor,
		OverheadMemoryBytes: p.Overhead.MemoryBytes,
		OverheadTasks:       p.Overhead.Tasks,
		OverheadCpus:        p.Overhead.CPUs,
		ExecutableSha256:    p.ExecutableSHA256,
	}
	for _, f := range p.BootFiles {
		out.BootFiles = append(out.BootFiles, &pb.BlockDevice{Path: f.Host, InPath: f.In, ReadOnly: f.ReadOnly, Sha256: f.SHA256})
	}
	return out
}

// logTail returns what the supervisor wrote to its log, for an error.
func logTail(dir string) string {
	raw, err := os.ReadFile(statedir.SupervisorLog(dir))
	if err != nil {
		return err.Error()
	}
	s := strings.TrimSpace(string(raw))
	if len(s) > 512 {
		s = s[len(s)-512:]
	}
	return s
}
