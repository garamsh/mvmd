package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
	"google.golang.org/protobuf/proto"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// confineCommand is the argument that makes this binary the confining child of
// a supervisor instead of a supervisor. The supervisor starts it with the
// namespaces and the cgroup the VMM is to have, the request on its standard
// input, the tap descriptor, if any, as descriptor 3 and the status pipe as
// descriptor 4.
const confineCommand = "confine"

const (
	// tapFD is the descriptor the VMM is passed its tap as.
	tapFD = 3
	// statusFD is the pipe the child reports a failure on. It closes at
	// execve, which is how the supervisor learns the VMM was started.
	statusFD = 4

	// statusReady is written to the status pipe as the last act before the
	// execve, and statusFailed before the text of a failure.
	statusReady  = 'R'
	statusFailed = 'E'
)

// confineMain runs the confinement steps and execs the VMM. It returns only on
// a failure, which it reports on the status pipe. The steps run on one thread,
// since capabilities, no_new_privs, Landlock and seccomp are the thread's, and
// the exec is made from it.
func confineMain(args []string) int {
	runtime.LockOSThread()
	status := os.NewFile(statusFD, "status")
	unix.CloseOnExec(statusFD)
	err := func() (err error) {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		if len(args) != 1 {
			return errors.New("the confinement is given the directory of the root and nothing else")
		}
		return confine(args[0], status)
	}()
	fmt.Fprintf(status, "%c%v", statusFailed, err)
	return 1
}

// confine applies the steps of ADR 0015 in its order, each failing the start
// before the VMM runs, and execs the VMM. It returns only the error of the step
// that failed.
func confine(newroot string, status *os.File) error {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	var req pb.StartRequest
	if err := proto.Unmarshal(raw, &req); err != nil {
		return fmt.Errorf("read the request: %w", err)
	}
	p := req.GetProcess()

	if err := openDescriptors(p.GetTapByDescriptor()); err != nil {
		return fmt.Errorf("open the descriptors: %w", err)
	}
	if err := enterNamespaces(newroot, &req); err != nil {
		return fmt.Errorf("enter the namespaces and build the root: %w", err)
	}
	if err := dropPrivileges(req.GetUid(), req.GetGid()); err != nil {
		return fmt.Errorf("drop capabilities and change user: %w", err)
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no_new_privs: %w", err)
	}
	if err := enforceLandlock(landlockRules(&req)); err != nil {
		return fmt.Errorf("enforce Landlock: %w", err)
	}
	if err := installBaseline(); err != nil {
		return fmt.Errorf("install the seccomp baseline: %w", err)
	}
	argv := append([]string{p.GetExecutable()}, p.GetArgs()...)
	if _, err := status.Write([]byte{statusReady}); err != nil {
		return fmt.Errorf("report to the supervisor: %w", err)
	}
	if err := unix.Exec(p.GetExecutable(), argv, nil); err != nil {
		return fmt.Errorf("execve %s: %w", p.GetExecutable(), err)
	}
	return errors.New("execve returned")
}

// openDescriptors makes the descriptors the VMM is passed ready: the tap, which
// the supervisor opened in the host's namespace, is checked to be there and
// stays open across the exec, and the VMM's standard input is /dev/null, which
// the root will not have.
func openDescriptors(tap bool) error {
	if tap {
		flags, err := unix.FcntlInt(tapFD, unix.F_GETFD, 0)
		if err != nil {
			return errNoDescriptor
		}
		if flags&unix.FD_CLOEXEC != 0 {
			return errors.New("the tap descriptor would close at the exec")
		}
	}
	null, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer unix.Close(null)
	if err := unix.Dup3(null, 0, 0); err != nil {
		return fmt.Errorf("replace the standard input: %w", err)
	}
	return nil
}

// enterNamespaces completes the namespaces the supervisor created the child
// in, and builds the root. A new network namespace holds only its loopback.
func enterNamespaces(newroot string, req *pb.StartRequest) error {
	if newNetworkNamespace(req) {
		if err := bringUpLoopback(); err != nil {
			return err
		}
	}
	return buildRoot(newroot, req)
}

// landlockRules grants the paths of the root as ADR 0015 decides: the
// executable to run, each boot file as its mode says, the files the Driver
// names to read, the device nodes it names to open and send ioctls to, and the
// run directory to use.
func landlockRules(req *pb.StartRequest) []fsRule {
	p := req.GetProcess()
	rules := []fsRule{{p.GetExecutable(), accessExecutable}, {req.GetRunDir(), accessRunDir}}
	for _, f := range p.GetBootFiles() {
		access := uint64(accessFile)
		if f.GetReadOnly() {
			access = accessRead
		}
		rules = append(rules, fsRule{f.GetPath(), access})
	}
	for _, f := range p.GetReadFiles() {
		rules = append(rules, fsRule{f, accessRead})
	}
	for _, n := range p.GetDeviceNodes() {
		rules = append(rules, fsRule{n, accessDevice})
	}
	return rules
}

var errNoDescriptor = errors.New("the tap descriptor 3 is not open")
