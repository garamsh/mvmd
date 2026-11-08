package supervisor

import (
	"os"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

// spawnSelf runs this test binary to its end, which matches no test, with the
// clone flags given.
func spawnSelf(flags uintptr) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	pid, err := syscall.ForkExec(self, []string{self, "-test.run=^$"}, &syscall.ProcAttr{
		Files: []uintptr{0, 2, 2}, // what it prints is not the case's report
		Env:   []string{},
		Sys:   &syscall.SysProcAttr{Cloneflags: flags},
	})
	if err != nil {
		return err
	}
	var ws syscall.WaitStatus
	_, err = syscall.Wait4(pid, &ws, 0, nil)
	return err
}

func init() {
	cases["seccomp"] = func([]byte) (map[string]string, error) {
		res := map[string]string{}
		try := func(phase string) {
			res[phase+" unshare"] = errName(unix.Unshare(0))
			res[phase+" setns"] = errName(unix.Setns(-1, 0))
			_, _, errno := unix.Syscall(unix.SYS_CLONE3, 0, 0, 0)
			res[phase+" clone3"] = errName(errno)
			res[phase+" clone with a namespace flag"] = errName(spawnSelf(syscall.CLONE_NEWUSER))
			res[phase+" clone without one"] = errName(spawnSelf(0))
		}
		try("before")
		if err := noNewPrivs(); err != nil {
			return nil, err
		}
		if err := installBaseline(); err != nil {
			return nil, err
		}
		try("after")
		return res, nil
	}
}

// What the baseline answers, each beside the same call before it. unshare(0)
// and setns(-1) change nothing and fail on their own only at EBADF, so an EPERM
// is the filter's; the clone with a namespace flag is the one an unprivileged
// process may be refused for another reason, so its control is that it is not
// refused with EPERM before.
func TestInstallBaseline_AnswersTheNamespaceCalls(t *testing.T) {
	res, err := child(t, "seccomp", nil)
	require.NoError(t, err)

	assert.Equal(t, "ok", res["before unshare"], "control")
	assert.Equal(t, "EPERM", res["after unshare"])
	assert.Equal(t, "EBADF", res["before setns"], "control")
	assert.Equal(t, "EPERM", res["after setns"])
	assert.NotEqual(t, "ENOSYS", res["before clone3"], "control: the kernel has clone3")
	assert.Equal(t, "ENOSYS", res["after clone3"])
	assert.NotEqual(t, "EPERM", res["before clone with a namespace flag"], "control")
	assert.Equal(t, "EPERM", res["after clone with a namespace flag"])
	assert.Equal(t, "ok", res["before clone without one"], "control")
	assert.Equal(t, "ok", res["after clone without one"], "a clone with no namespace flag is allowed")
}
