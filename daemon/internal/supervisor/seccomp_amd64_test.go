package supervisor

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// x32 is the bit that makes an amd64 system call number an x32 one, which
// shares the entry and numbers its calls apart.
const x32 = 0x40000000

func init() {
	cases["x32 before"] = func([]byte) (map[string]string, error) {
		_, _, errno := unix.Syscall(x32|unix.SYS_GETPID, 0, 0, 0)
		return map[string]string{"x32 getpid": errName(errno)}, nil
	}
	cases["x32 after"] = func([]byte) (map[string]string, error) {
		if err := noNewPrivs(); err != nil {
			return nil, err
		}
		if err := installBaseline(); err != nil {
			return nil, err
		}
		_, _, errno := unix.Syscall(x32|unix.SYS_GETPID, 0, 0, 0)
		return map[string]string{"x32 getpid": errName(errno)}, nil
	}
}

// A call through the x32 numbers would reach unshare and clone unfiltered, so
// the baseline ends a process that makes one. The control is the same call
// before the filter, which the kernel answers, ENOSYS where it has no x32,
// without ending the process.
func TestInstallBaseline_EndsAProcessUsingTheX32Numbers(t *testing.T) {
	before, err := child(t, "x32 before", nil)
	require.NoError(t, err, "control: the call does not end the process before the filter")
	t.Logf("control: x32 getpid before the filter: %s", before["x32 getpid"])

	_, err = child(t, "x32 after", nil)
	require.Error(t, err)
	var exit *exec.ExitError
	require.ErrorAs(t, err, &exit)
	ws := exit.Sys().(syscall.WaitStatus)
	assert.True(t, ws.Signaled() && ws.Signal() == syscall.SIGSYS, "the process was ended by SIGSYS: %v", err)
}
