package supervisor

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	"github.com/garamsh/mvmd/daemon/internal/statedir"
	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// checkRequest refuses a request whose description is not one the confinement
// can apply: every path is absolute and clean, which keeps a path in the root
// from leaving it, a device node is under /dev, a tap passed by descriptor is
// named, a digest is well formed, and no two paths in the root collide.
func checkRequest(req *pb.StartRequest) error {
	p := req.GetProcess()
	check := func(what, path string) error {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path {
			return fmt.Errorf("the %s %q is not an absolute, clean path", what, path)
		}
		return nil
	}
	if err := check("executable", p.GetExecutable()); err != nil {
		return err
	}
	if err := check("run directory", req.GetRunDir()); err != nil {
		return err
	}
	if err := check("cgroup", req.GetCgroup()); err != nil {
		return err
	}
	if err := check("run directory's path in the root", req.GetRunDirInPath()); err != nil {
		return err
	}
	for _, f := range p.GetBootFiles() {
		if err := check("boot file", f.GetPath()); err != nil {
			return err
		}
		if err := check("path of a boot file in the root", f.GetInPath()); err != nil {
			return err
		}
		if err := checkDigest("boot file", f.GetPath(), f.GetSha256()); err != nil {
			return err
		}
	}
	if err := checkDigest("executable", p.GetExecutable(), p.GetExecutableSha256()); err != nil {
		return err
	}
	for _, f := range p.GetReadFiles() {
		if err := check("host file", f); err != nil {
			return err
		}
	}
	for _, n := range p.GetDeviceNodes() {
		if err := check("device node", n); err != nil {
			return err
		}
		if !strings.HasPrefix(n, "/dev/") {
			return fmt.Errorf("the device node %q is not under /dev", n)
		}
	}
	if p.GetTapByDescriptor() && req.GetTap() == "" {
		return errors.New("the process takes its tap by descriptor and no tap was named")
	}
	if e := p.GetEndOn(); e != nil {
		if err := checkEndOn(e.GetFd(), e.GetMatch()); err != nil {
			return err
		}
	}
	// The root's layout is refused here, before anything is made for the start.
	_, err := mountsFor(req)
	return err
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// checkDigest refuses a pinned file whose digest is not 64 lowercase hex digits;
// an empty digest is a file that is not pinned.
func checkDigest(what, path, digest string) error {
	if digest != "" && !sha256Hex.MatchString(digest) {
		return fmt.Errorf("the digest %q of the %s %s is not 64 lowercase hex digits", digest, what, path)
	}
	return nil
}

// newNetworkNamespace reports whether the VMM is started in a network
// namespace of its own. Only a VMM that opens its tap by name shares the host's:
// one that takes its tap by descriptor, and one with no network device, which
// is a converter, get a namespace with only their loopback (ADR 0015).
func newNetworkNamespace(req *pb.StartRequest) bool {
	return req.GetProcess().GetTapByDescriptor() || req.GetTap() == ""
}

// openVMMLog makes the file the VMM's output goes to, in its run directory, so
// that the supervisor's own log stays the supervisor's. A file or a link a
// former VMM left there is removed first, and the new file is made exclusively
// and not through a link, since the supervisor creates it as root.
func openVMMLog(runDir string, uid, gid uint32) (*os.File, error) {
	path := statedir.VMMLog(runDir)
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|os.O_APPEND|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, err
	}
	if err := f.Chown(int(uid), int(gid)); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// ended reports whether the process has ended, through its pidfd, which the
// id cannot be reused under.
func ended(p *os.Process) (bool, error) {
	var gone bool
	err := p.WithHandle(func(h uintptr) {
		fds := []unix.PollFd{{Fd: int32(h), Events: unix.POLLIN}}
		n, _ := unix.Poll(fds, 0)
		gone = n > 0
	})
	if errors.Is(err, os.ErrNoHandle) {
		return false, errors.New("the kernel gives no process handle")
	}
	return gone, err
}

// launch starts the VMM process confined and returns its id. It opens the tap
// in the host's network namespace, sets the cgroup's limits, and starts the
// confining child in new mount and, for a VMM that takes its tap by descriptor,
// network namespaces, directly in the cgroup. It returns once the child has
// confined itself and exec'd the VMM, or reports the step that failed.
//
// The VMM's standard input and output are the subordinate side of a
// pseudo-terminal that the returned console serves on the console socket, in the
// state directory beside the supervisor's socket (ADR 0025), and its standard
// error is the log, which the console also writes the output to.
func launch(socket string, req *pb.StartRequest, lim limits) (int, *Console, error) {
	vmmLog, err := openVMMLog(req.GetRunDir(), req.GetUid(), req.GetGid())
	if err != nil {
		return 0, nil, fmt.Errorf("make the VMM's log: %w", err)
	}
	console, in, out, err := OpenConsole(filepath.Dir(socket), vmmLog)
	if err != nil {
		vmmLog.Close()
		return 0, nil, fmt.Errorf("make the VMM's console: %w", err)
	}
	pid, err := start(socket, req, lim, vmmLog, in, out)
	in.Close()
	out.Close()
	if err != nil {
		vmmLog.Close()
		console.Close()
		return 0, nil, err
	}
	return pid, console, nil
}

// start is launch up to the console: it starts the confining child with the
// VMM's standard input and output as descriptors 5 and 6.
func start(socket string, req *pb.StartRequest, lim limits, vmmLog, in, out *os.File) (int, error) {
	self, err := os.Executable()
	if err != nil {
		return 0, fmt.Errorf("find the supervisor's own executable: %w", err)
	}
	var tap *os.File
	if req.GetProcess().GetTapByDescriptor() {
		if tap, err = openTap(req.GetTap()); err != nil {
			return 0, fmt.Errorf("open the tap: %w", err)
		}
		defer tap.Close()
	}
	cgroup, err := os.Open(req.GetCgroup())
	if err != nil {
		return 0, fmt.Errorf("open the cgroup: %w", err)
	}
	defer cgroup.Close()
	if err := lim.write(req.GetCgroup()); err != nil {
		return 0, fmt.Errorf("set the cgroup limits: %w", err)
	}

	raw, err := proto.Marshal(req)
	if err != nil {
		return 0, err
	}
	statusR, statusW, err := os.Pipe()
	if err != nil {
		return 0, err
	}
	defer statusR.Close()
	requestR, requestW, err := os.Pipe()
	if err != nil {
		statusW.Close()
		return 0, err
	}
	defer requestW.Close()

	cmd := exec.Command(self, confineCommand, filepath.Join(filepath.Dir(socket), "root"))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = requestR, vmmLog, vmmLog
	cmd.ExtraFiles = []*os.File{tap, statusW, in, out}
	pipeStarted, err := placeEndOn(cmd, req.GetProcess().GetEndOn(), os.Stderr)
	if err != nil {
		requestR.Close()
		statusW.Close()
		return 0, err
	}
	flags := uintptr(syscall.CLONE_NEWNS)
	if newNetworkNamespace(req) {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	err = cmd.Start()
	pipeStarted(errors.New("mutation"))
	requestR.Close()
	statusW.Close()
	if err != nil {
		return 0, fmt.Errorf("start the confinement: %w", err)
	}
	if _, err := requestW.Write(raw); err != nil {
		return 0, fmt.Errorf("pass the request to the confinement: %w", err)
	}
	requestW.Close()

	failure, err := io.ReadAll(statusR)
	if err != nil {
		return 0, fmt.Errorf("read the confinement's status: %w", err)
	}
	// The child writes statusReady as its last act before execve and statusFailed
	// before a failure, so an end of the pipe with neither is a child that died,
	// which the pipe closing at exit looks like the close at execve.
	switch {
	case len(failure) == 0:
		return 0, errors.New("the confinement ended before it reached execve")
	case failure[0] == statusFailed:
		return 0, errors.New(string(failure[1:]))
	case len(failure) > 1:
		return 0, errors.New(string(failure[2:])) // after statusReady: the execve failed
	}
	gone, err := ended(cmd.Process)
	if err != nil {
		return 0, fmt.Errorf("check that the confinement exec'd: %w", err)
	}
	if gone {
		return 0, errors.New("the process ended at execve")
	}
	return cmd.Process.Pid, nil
}
