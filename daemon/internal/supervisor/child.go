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
	// inputFD and outputFD are the two opens of the console terminal's
	// subordinate side the VMM is given, which become its standard input and
	// output.
	inputFD  = 5
	outputFD = 6

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

	if err := openDescriptors(p.GetTapByDescriptor(), p.GetEndOn()); err != nil {
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

// openDescriptors makes the descriptors the VMM is passed ready: the tap and the
// event pipe's write end (ADR 0026) are checked to be there and to stay open
// across the exec, and the console terminal (ADR 0025) replaces descriptors 0
// and 1 and is closed where it was passed.
func openDescriptors(tap bool, events *pb.EndOn) error {
	if tap {
		if err := checkInherited(tapFD, "tap"); err != nil {
			return err
		}
	}
	if events != nil {
		if err := checkInherited(int(events.GetFd()), "event pipe"); err != nil {
			return err
		}
	}
	for _, d := range []struct{ from, to int }{{inputFD, 0}, {outputFD, 1}} {
		if err := unix.Dup3(d.from, d.to, 0); err != nil {
			return fmt.Errorf("replace descriptor %d with the console terminal: %w", d.to, err)
		}
		_ = unix.Close(d.from)
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
	rules := []fsRule{{p.GetExecutable(), accessExecutable}, {req.GetRunDirInPath(), accessRunDir}}
	for _, f := range p.GetBootFiles() {
		access := uint64(accessFile)
		if f.GetReadOnly() {
			access = accessRead
		}
		rules = append(rules, fsRule{f.GetInPath(), access})
	}
	for _, f := range p.GetReadFiles() {
		rules = append(rules, fsRule{f, accessRead})
	}
	for _, n := range p.GetDeviceNodes() {
		rules = append(rules, fsRule{n, accessDevice})
	}
	return rules
}

// checkInherited verifies that the descriptor is open and stays open across the
// exec.
func checkInherited(fd int, what string) error {
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("the %s descriptor %d is not open", what, fd)
	}
	if flags&unix.FD_CLOEXEC != 0 {
		return fmt.Errorf("the %s descriptor %d would close at the exec", what, fd)
	}
	return nil
}
