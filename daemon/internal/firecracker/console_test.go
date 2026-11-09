package firecracker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

// What a console connection and a log are is vmconsole's, and its tests hold
// it. These hold the wiring: the log is vmm.log and the socket is the console
// socket, both of the Machine's run directory.

func TestConsoleLog_IsTheRunDirectorysVMMLog(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "control: there is no log yet")

	require.NoError(t, os.WriteFile(filepath.Join(n.run, "vmm.log"), []byte("boot\n"), 0o600))
	r, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err)
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "boot\n", string(b))
}

func TestConsole_IsASessionOnTheRunDirectorysConsoleSocket(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.Console(context.Background(), "m1")
	require.ErrorIs(t, err, machine.ErrConsoleUnavailable, "control: nothing listens yet")

	l := listenUnix(t, statedir.ConsoleSocket(statedir.Of(n.run)))
	t.Cleanup(func() { _ = l.Close() })
	accepted := make(chan io.ReadWriteCloser, 1)
	go func() {
		c, err := l.Accept()
		if err == nil {
			accepted <- c
		}
	}()

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
	assert.Equal(t, "login: ", string(buf))
}
