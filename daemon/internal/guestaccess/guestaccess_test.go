package guestaccess_test

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/guestaccess"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

// What a console connection, a log and the vsock handshake are is vmconsole's
// and vsockdial's, and their tests hold them. These hold the wiring: the log is
// vmm.log, the console socket is beside the run directory, the vsock socket is
// the run directory's vsock.sock, and the port is the caller's.

type dirs struct{ dir string }

func (d dirs) RunDir(machine.MachineID) (string, error) { return d.dir, nil }

type failingDirs struct{}

func (failingDirs) RunDir(machine.MachineID) (string, error) {
	return "", errors.New("no such Machine")
}

// access is an Access over a run directory in a state directory of its own,
// as the supervisor makes one.
func access(t *testing.T) (guestaccess.Access, string) {
	t.Helper()
	run := filepath.Join(t.TempDir(), "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	return guestaccess.Access{Dirs: dirs{run}}, run
}

// listen binds a socket at path through a descriptor of its directory, as the
// supervisor does.
func listen(t *testing.T, path string) *net.UnixListener {
	t.Helper()
	fd, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = unix.Close(fd) })
	l, err := net.Listen("unix", fmt.Sprintf("/proc/self/fd/%d/%s", fd, filepath.Base(path)))
	require.NoError(t, err)
	t.Cleanup(func() { _ = l.Close() })
	return l.(*net.UnixListener)
}

func TestConsoleLog_IsTheRunDirectorysVMMLog(t *testing.T) {
	a, run := access(t)
	_, err := a.ConsoleLog(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "control: there is no log yet")

	require.NoError(t, os.WriteFile(filepath.Join(run, "vmm.log"), []byte("boot\n"), 0o600))
	r, err := a.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err)
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "boot\n", string(b))
}

func TestConsole_IsASessionOnTheRunDirectorysConsoleSocket(t *testing.T) {
	a, run := access(t)
	_, err := a.Console(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "control: nothing listens yet")

	l := listen(t, statedir.ConsoleSocket(statedir.Of(run)))
	accepted := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()

	s, err := a.Console(context.Background(), "m1")
	require.NoError(t, err)
	defer s.Close()
	guest := <-accepted
	defer guest.Close()
	_, err = guest.Write([]byte("login: "))
	require.NoError(t, err)
	buf := make([]byte, 7)
	_, err = io.ReadFull(s, buf)
	require.NoError(t, err)
	assert.Equal(t, "login: ", string(buf))
}

func TestGuestConn_AsksTheRunDirectorysVsockSocketForThePort(t *testing.T) {
	a, run := access(t)
	_, err := a.GuestConn(context.Background(), "m1", 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable, "control: no VMM listens yet")

	l := listen(t, filepath.Join(run, "vsock.sock"))
	got := make(chan string, 1)
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
		_, _ = io.WriteString(conn, "OK 1073741824\nstream")
		_, _ = io.Copy(io.Discard, conn)
	}()

	conn, err := a.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "CONNECT 1024\n", <-got)
	buf := make([]byte, 6)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "stream", string(buf))
}

func TestAnOperationReturnsTheRunDirectoryLookupsFailure(t *testing.T) {
	a := guestaccess.Access{Dirs: failingDirs{}}

	_, err := a.Console(context.Background(), "m1")
	assert.ErrorContains(t, err, "no such Machine")
	_, err = a.ConsoleLog(context.Background(), "m1")
	assert.ErrorContains(t, err, "no such Machine")
	_, err = a.GuestConn(context.Background(), "m1", 1024)
	assert.ErrorContains(t, err, "no such Machine")
}
