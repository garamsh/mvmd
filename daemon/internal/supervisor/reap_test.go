package supervisor

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitReason_NamesAnExitStatusAndASignal(t *testing.T) {
	wait := func(name string, args ...string) syscall.WaitStatus {
		cmd := exec.Command(name, args...)
		_ = cmd.Run()
		require.NotNil(t, cmd.ProcessState)
		return cmd.ProcessState.Sys().(syscall.WaitStatus)
	}
	assert.Equal(t, "exit status 3", waitReason(wait("sh", "-c", "exit 3")))
	assert.Equal(t, "signal: killed", waitReason(wait("sh", "-c", "kill -9 $$")))
}
