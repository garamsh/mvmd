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
	envConverterSHA    = "MVMD_E2E_CONVERTER_INITRD_SHA256"
	envDriver          = "MVMD_E2E_DRIVER"
	envFirecracker     = "MVMD_FIRECRACKER"
	envFirecrackerSHA  = "MVMD_FIRECRACKER_SHA256"
	envCloudHypervisor = "MVMD_CLOUD_HYPERVISOR"
	envCloudHypSHA     = "MVMD_CLOUD_HYPERVISOR_SHA256"
	envKernel          = "MVMD_KERNEL"
	envKernelSHA       = "MVMD_KERNEL_SHA256"
	envGuestInitrd     = "MVMD_INITRD"
	envGuestInitrdSHA  = "MVMD_INITRD_SHA256"
	envFsckErofs       = "FSCK_EROFS"
)

// vmm is what differs between the Drivers the tests run under: the name the node's configuration
// selects and keys the Driver's section by, and the inputs naming its binary and that binary's
// digest. The kernel and the initrds are the same for both.
type vmm struct{ driver, section, binaryEnv, binarySHAEnv string }

var vmms = map[string]vmm{
	"firecracker":      {"firecracker", "firecracker", envFirecracker, envFirecrackerSHA},
	"cloud-hypervisor": {"cloud-hypervisor", "cloud_hypervisor", envCloudHypervisor, envCloudHypSHA},
}

// driver is the Driver the tests run under, chosen by MVMD_E2E_DRIVER; it is the one input that
// selects it. A name that is not one of the two is refused in TestMain, never defaulted.
var driver vmm

var requiredInputs = []string{
	envMvmd, envSupervisor, envConverterInitrd, envDriver,
	envKernel, envKernelSHA, envGuestInitrd, envGuestInitrdSHA, envConverterSHA, envFsckErofs,
}

// TestMain checks the inputs, then runs the tests as root. The daemon starts supervisors, makes
// cgroups and mounts, which only root does; a user who is not root runs the tests again under
// passwordless sudo, as the other real tests of the repository do.
func TestMain(m *testing.M) {
	var missing []string
	inputs := requiredInputs
	if v, ok := vmms[os.Getenv(envDriver)]; ok {
		driver = v
		inputs = append(inputs[:len(inputs):len(inputs)], v.binaryEnv, v.binarySHAEnv)
	} else if os.Getenv(envDriver) != "" {
		fmt.Fprintf(os.Stderr, "%s is %q, not firecracker or cloud-hypervisor\n", envDriver, os.Getenv(envDriver))
		os.Exit(1)
	}
	for _, name := range inputs {
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
