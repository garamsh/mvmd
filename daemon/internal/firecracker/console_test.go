package firecracker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func readAll(t *testing.T, r io.ReadCloser) string {
	t.Helper()
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

func TestConsoleLog_IsWhatTheSupervisorCapturedUpToTheCall(t *testing.T) {
	n := newNode(t)
	log := filepath.Join(n.run, "vmm.log")
	require.NoError(t, os.WriteFile(log, []byte("boot\n"), 0o600))

	r, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err)

	// What the console writes after the call is not in what the call returned.
	f, err := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString("later\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	assert.Equal(t, "boot\n", readAll(t, r))

	r, err = n.driver.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err)
	assert.Equal(t, "boot\nlater\n", readAll(t, r), "control: a later call holds it")
}

func TestConsoleLog_IsUnavailableWhenThereIsNoLog(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable)
}

func TestConsoleLog_NeverFollowsALinkTheVMMLeftInItsDirectory(t *testing.T) {
	n := newNode(t)
	secret := filepath.Join(t.TempDir(), "secret")
	require.NoError(t, os.WriteFile(secret, []byte("host secret"), 0o600))
	log := filepath.Join(n.run, "vmm.log")

	require.NoError(t, os.WriteFile(log, []byte("console"), 0o600))
	r, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err, "control: a regular file is read")
	assert.Equal(t, "console", readAll(t, r))

	require.NoError(t, os.Remove(log))
	require.NoError(t, os.Symlink(secret, log))
	r, err = n.driver.ConsoleLog(context.Background(), "m1")
	if err == nil {
		t.Fatalf("a link to a host file was read: %q", readAll(t, r))
	}
}

func TestConsoleLog_IsUnavailableForAFileThatIsNotRegular(t *testing.T) {
	n := newNode(t)
	require.NoError(t, syscall.Mkfifo(filepath.Join(n.run, "vmm.log"), 0o600))
	_, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable)
}

func TestConsoleLog_HonoursItsContext(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.WriteFile(filepath.Join(n.run, "vmm.log"), []byte("x"), 0o600))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := n.driver.ConsoleLog(ctx, "m1")
	require.ErrorIs(t, err, context.Canceled)
}

func TestConsole_IsUnavailableBecauseTheVMMsInputReachesNoSession(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Console(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable)
}
