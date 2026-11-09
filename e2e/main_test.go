//go:build firecracker_real

package e2e_test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The inputs `make test-e2e` builds or fetches and names to the tests. A missing one fails the
// run: a test that cannot reach the real thing is not a pass.
const (
	envMvmd            = "MVMD_E2E_MVMD"
	envSupervisor      = "MVMD_E2E_SUPERVISOR"
	envConverterInitrd = "MVMD_E2E_CONVERTER_INITRD"
	envFirecracker     = "MVMD_FIRECRACKER"
	envFirecrackerSHA  = "MVMD_FIRECRACKER_SHA256"
	envKernel          = "MVMD_KERNEL"
	envKernelSHA       = "MVMD_KERNEL_SHA256"
	envGuestInitrd     = "MVMD_INITRD"
	envFsckErofs       = "FSCK_EROFS"
)

var requiredInputs = []string{
	envMvmd, envSupervisor, envConverterInitrd, envFirecracker, envFirecrackerSHA,
	envKernel, envKernelSHA, envGuestInitrd, envFsckErofs,
}

// TestMain checks the inputs, then runs the tests as root. The daemon starts supervisors, makes
// cgroups and mounts, which only root does; a user who is not root runs the tests again under
// passwordless sudo, as the other real tests of the repository do.
func TestMain(m *testing.M) {
	var missing []string
	for _, name := range requiredInputs {
		if os.Getenv(name) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		fmt.Fprintf(os.Stderr, "the end-to-end tests need %s; run them through `make test-e2e`\n", strings.Join(missing, ", "))
		os.Exit(1)
	}
	if os.Geteuid() != 0 {
		os.Exit(reexecUnderSudo())
	}
	os.Exit(m.Run())
}

// reexecUnderSudo runs this test binary again as root, with the same environment and arguments,
// and returns its exit code.
func reexecUnderSudo() int {
	if err := exec.Command("sudo", "-n", "true").Run(); err != nil {
		fmt.Fprintln(os.Stderr, "the end-to-end tests run as root, and this user is not root and has no passwordless sudo:", err)
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
