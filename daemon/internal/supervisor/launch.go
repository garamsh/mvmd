package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"

	"google.golang.org/protobuf/proto"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// checkRequest refuses a request whose description is not one the confinement
// can apply: every path is absolute and clean, a device node is under /dev, and
// a tap passed by descriptor is named.
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
	for _, f := range p.GetBootFiles() {
		if err := check("boot file", f.GetPath()); err != nil {
			return err
		}
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
	return nil
}

// newNetworkNamespace reports whether the VMM is started in a network
// namespace of its own.
func newNetworkNamespace(req *pb.StartRequest) bool { return req.GetProcess().GetTapByDescriptor() }

// launch starts the VMM process confined and returns its id. It opens the tap
// in the host's network namespace, sets the cgroup's limits, and starts the
// confining child in new mount and, for a VMM that takes its tap by descriptor,
// network namespaces, directly in the cgroup. It returns once the child has
// confined itself and exec'd the VMM, or reports the step that failed.
func launch(socket string, req *pb.StartRequest, lim limits) (int, error) {
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
	cmd.Stdin, cmd.Stdout, cmd.Stderr = requestR, os.Stdout, os.Stderr
	cmd.ExtraFiles = []*os.File{tap, statusW} // descriptors 3 (tapFD) and 4 (statusFD)
	flags := uintptr(syscall.CLONE_NEWNS)
	if tap != nil {
		flags |= syscall.CLONE_NEWNET
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: flags, UseCgroupFD: true, CgroupFD: int(cgroup.Fd())}
	err = cmd.Start()
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
	if len(failure) > 0 {
		return 0, errors.New(string(failure))
	}
	return cmd.Process.Pid, nil
}
