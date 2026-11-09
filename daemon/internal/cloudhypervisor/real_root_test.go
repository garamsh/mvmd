//go:build cloud_hypervisor_real

package cloudhypervisor_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The VMM of the real tests sees the files of its description at the fixed
// paths in its root (ADR 0019), as the supervisor gives them, so the tests
// run it in a root of that shape. It stands in for the supervisor's root,
// bind-mounting each file at its path and the device nodes the description
// names, with no /proc and no /sys. It confines nothing else: the user, the
// capabilities and the filters are the supervisor's, and internal/supervision
// tests them.
//
// The root is a mount namespace inside a user namespace of the user running
// the tests, which needs no privilege. Where the host refuses mounts in such a
// namespace (Ubuntu 24.04 restricts them by default), the tests run again as
// root under passwordless sudo, as internal/supervision's do, and the root is a
// mount namespace of root's.

const (
	rootEnv      = "MVMD_REAL_ROOT"
	rootCheckEnv = "MVMD_REAL_ROOT_CHECK"
)

func TestMain(m *testing.M) {
	if raw := os.Getenv(rootEnv); raw != "" {
		fmt.Fprintln(os.Stderr, "enter the root:", enterRoot(raw))
		os.Exit(1)
	}
	if os.Getenv(rootCheckEnv) != "" {
		os.Exit(checkMounts())
	}
	if os.Geteuid() != 0 && !userNamespaceMounts() {
		os.Exit(reexecUnderSudo())
	}
	os.Exit(m.Run())
}

// namespaces are the flags and ID maps a child is started with to be able to
// mount: a user namespace mapping the user to root unless it is root already.
func namespaces() *syscall.SysProcAttr {
	if os.Geteuid() == 0 {
		return &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWNS}
	}
	return &syscall.SysProcAttr{
		Cloneflags:  syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getuid(), Size: 1}},
		GidMappings: []syscall.SysProcIDMap{{ContainerID: 0, HostID: os.Getgid(), Size: 1}},
	}
}

// userNamespaceMounts reports whether a child in new user and mount namespaces
// can mount.
func userNamespaceMounts() bool {
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), rootCheckEnv+"=1")
	cmd.SysProcAttr = namespaces()
	return cmd.Run() == nil
}

// checkMounts is the child of userNamespaceMounts: it mounts a tmpfs.
func checkMounts() int {
	dir, err := os.MkdirTemp("", "chcheck")
	if err != nil {
		return 1
	}
	defer os.Remove(dir)
	if unix.Mount("tmpfs", dir, "tmpfs", 0, "") != nil {
		return 1
	}
	return 0
}

// reexecUnderSudo runs this test binary again as root, with the same
// environment and arguments, and returns its exit code.
func reexecUnderSudo() int {
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "the real tests need to mount in a root of their own, which this host refuses to a user namespace, and passwordless sudo is not available either:", err)
		return 1
	}
	cmd := exec.Command("sudo", append(append([]string{"-n", "env"}, os.Environ()...), os.Args...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exit.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// inRoot returns the command that runs the described process in its root,
// started in the namespaces above; the caller sets its output. The description
// is the one the lifecycle gives the supervisor, so it names the run directory
// (machine.ProcessDescription.For).
func inRoot(p machine.ProcessDescription) (*exec.Cmd, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), rootEnv+"="+string(raw))
	cmd.SysProcAttr = namespaces()
	return cmd, nil
}

// enterRoot builds the root in this process, pivots into it and execs the
// process. It returns only the error that stopped it.
func enterRoot(raw string) error {
	var p machine.ProcessDescription
	if err := json.Unmarshal([]byte(raw), &p); err != nil {
		return err
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make the mounts private: %w", err)
	}
	newroot, err := os.MkdirTemp("", "chroot")
	if err != nil {
		return err
	}
	if err := unix.Mount("tmpfs", newroot, "tmpfs", 0, "mode=0755"); err != nil {
		return fmt.Errorf("mount the root: %w", err)
	}
	bind := func(host, in string) error {
		st, err := os.Stat(host)
		if err != nil {
			return err
		}
		dst := filepath.Join(newroot, in)
		if st.IsDir() {
			err = os.MkdirAll(dst, 0o755)
		} else if err = os.MkdirAll(filepath.Dir(dst), 0o755); err == nil {
			var f *os.File
			if f, err = os.OpenFile(dst, os.O_CREATE|os.O_WRONLY, 0o644); err == nil {
				err = f.Close()
			}
		}
		if err != nil {
			return err
		}
		if err := unix.Mount(host, dst, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind %s at %s: %w", host, in, err)
		}
		return nil
	}
	if err := bind(p.Executable, p.Executable); err != nil {
		return err
	}
	for _, f := range p.BootFiles {
		if err := bind(f.Host, f.In); err != nil {
			return err
		}
	}
	for _, n := range p.DeviceNodes {
		if err := bind(n, n); err != nil {
			return err
		}
	}
	if err := bind(p.RunDir, machine.InRunDir); err != nil {
		return err
	}
	if err := os.Chdir(newroot); err != nil {
		return err
	}
	if err := unix.PivotRoot(".", "."); err != nil {
		return fmt.Errorf("pivot into the root: %w", err)
	}
	if err := unix.Unmount(".", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount the host's root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	return unix.Exec(p.Executable, append([]string{p.Executable}, p.Args...), nil)
}
