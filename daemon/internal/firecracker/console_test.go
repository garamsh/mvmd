package firecracker_test

import (
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
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

// serveConsole stands in for the supervisor's console socket, at the name the
// supervisor gives it in the Machine's state directory, and returns what it
// accepted.
func serveConsole(t *testing.T, n *node) <-chan net.Conn {
	t.Helper()
	l := listenUnix(t, statedir.ConsoleSocket(statedir.Of(n.run)))
	t.Cleanup(func() { _ = l.Close() })
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	return accepted
}

func TestConsole_IsASessionOnTheSupervisorsSocketBesideTheRunDirectory(t *testing.T) {
	n := newNode(t)
	accepted := serveConsole(t, n)

	s, err := n.driver.Console(context.Background(), "m1")
	require.NoError(t, err)
	defer s.Close()
	guest := <-accepted
	defer guest.Close()

	_, err = guest.Write([]byte("login: "))
	require.NoError(t, err)
	buf := make([]byte, 7)
	_, err = io.ReadFull(s, buf)
	require.NoError(t, err)
	assert.Equal(t, "login: ", string(buf), "what the guest writes is read from the session")

	_, err = s.Write([]byte("root\n"))
	require.NoError(t, err)
	in := make([]byte, 5)
	_, err = io.ReadFull(guest, in)
	require.NoError(t, err)
	assert.Equal(t, "root\n", string(in), "what is written to the session is the console's input")

	other, err := n.driver.Console(context.Background(), "m1")
	require.NoError(t, err, "a second session opens beside the first")
	require.NoError(t, other.Close())
	_, err = s.Write([]byte("x"))
	assert.NoError(t, err, "and closing it leaves the first as it was")
}

func TestConsole_IsUnavailableOnceTheSupervisorsSocketIsGoneOrRefuses(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Console(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "no socket")

	// The control: the same Driver, with a listener at the socket the
	// supervisor serves, opens a session. Without it the refusals here would
	// hold for a Driver that dials the wrong name.
	sock := statedir.ConsoleSocket(statedir.Of(n.run))
	l := listenUnix(t, sock)
	l.SetUnlinkOnClose(false)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	s, err := n.driver.Console(context.Background(), "m1")
	require.NoError(t, err, "a socket someone listens on")
	require.NoError(t, s.Close())

	require.NoError(t, l.Close())
	_, err = n.driver.Console(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "a socket nothing listens on")
}
