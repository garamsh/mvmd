package supervision

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hostPID maps the number a supervisor gives its child to the child's id in
// mvmd's namespace. This is internal because the mapping is the seam: a child
// in a new PID namespace is its init, number 1 there.
func TestHostPID_MapsAChildOfTheSupervisorToItsHostID(t *testing.T) {
	child := exec.Command("/bin/sleep", "100")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })

	got, ok := hostPID(os.Getpid(), 1)
	require.True(t, ok)
	assert.Equal(t, child.Process.Pid, got)

	// Controls: another number, and another parent, find nothing.
	_, ok = hostPID(os.Getpid(), 2)
	assert.False(t, ok, "no child is numbered 2")
	_, ok = hostPID(2147483647, 1)
	assert.False(t, ok, "the child of another parent is not found")
}

// stillTheChild is the check that turns a host id into a pidfd safely: after
// the open, the id must still name the supervisor's child with the same inner
// number, or the number was reused.
func TestStillTheChild_RechecksTheProcessBehindTheID(t *testing.T) {
	child := exec.Command("/bin/sleep", "100")
	child.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWPID}
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill(); _ = child.Wait() })
	pid := child.Process.Pid

	assert.True(t, stillTheChild(pid, os.Getpid(), 1))

	// Controls: another parent, another inner number, and a process that is
	// gone each fail the check.
	assert.False(t, stillTheChild(pid, 2147483647, 1), "another parent")
	assert.False(t, stillTheChild(pid, os.Getpid(), 2), "another inner number")
	_ = child.Process.Kill()
	_ = child.Wait()
	assert.False(t, stillTheChild(pid, os.Getpid(), 1), "the process is gone")
}
