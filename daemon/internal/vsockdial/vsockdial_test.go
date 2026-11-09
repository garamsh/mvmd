package vsockdial_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
	"github.com/garamsh/mvmd/daemon/internal/vsockdial"
)

func socketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "vsock.sock")
}

func listen(t *testing.T, path string) net.Listener {
	t.Helper()
	l, err := unixsock.Listen(path)
	require.NoError(t, err)
	return l
}

// fakeVsock serves the host side of a guest's vsock: it reads the connect
// request and answers with reply, then writes behind it. It records the
// request line it read.
func fakeVsock(t *testing.T, path, reply, behind string) <-chan string {
	t.Helper()
	ln := listen(t, path)
	t.Cleanup(func() { _ = ln.Close() })
	got := make(chan string, 1)
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		line, _ := bufio.NewReader(conn).ReadString('\n')
		got <- line
		_, _ = io.WriteString(conn, reply+behind)
		// Hold the stream open until the client goes.
		_, _ = io.Copy(io.Discard, conn)
	}()
	return got
}

func TestDial_AsksForThePortAndReadsTheAnswerBeforeAnyStreamByte(t *testing.T) {
	sock := socketPath(t)
	// The VMM answers with the host-side port it assigned, not the guest's.
	got := fakeVsock(t, sock, "OK 1073741824\n", "stream")

	conn, err := vsockdial.Dial(context.Background(), sock, machine.GuestAgentPort)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "CONNECT 1024\n", <-got)
	buf := make([]byte, 6)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "stream", string(buf), "the bytes behind the answer are the stream's, none consumed")
}

func TestDial_IsUnreachableWhenTheHostSideDoesNotAcknowledge(t *testing.T) {
	tests := []struct{ name, reply string }{
		{"the guest refuses and the VMM closes the connection", ""},
		{"an answer that is not OK", "NO 1024\n"},
		{"an OK without a number", "OK \n"},
		{"an OK with a word after it", "OK port\n"},
		{"an answer with no end of line", "OK 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := socketPath(t)
			fakeVsock(t, sock, "OK 7\n", "")
			conn, err := vsockdial.Dial(context.Background(), sock, 1024)
			require.NoError(t, err, "control: an acknowledged connect succeeds")
			_ = conn.Close()

			sock = socketPath(t)
			ln := listen(t, sock)
			defer ln.Close()
			go func() {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				_, _ = bufio.NewReader(c).ReadString('\n')
				_, _ = io.WriteString(c, tt.reply)
				_ = c.Close()
			}()
			_, err = vsockdial.Dial(context.Background(), sock, 1024)
			require.ErrorIs(t, err, machine.ErrGuestUnreachable)
		})
	}
}

func TestDial_IsUnreachableWhenNoVMMListens(t *testing.T) {
	sock := socketPath(t)
	_, err := vsockdial.Dial(context.Background(), sock, 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
}

func TestDial_StopsWaitingForAnAnswerWhenTheContextEnds(t *testing.T) {
	sock := socketPath(t)
	ln := listen(t, sock)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.Copy(io.Discard, c) // never answers
	}()

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := vsockdial.Dial(ctx, sock, 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestDial_LeavesNoDeadlineOnTheStreamItReturns(t *testing.T) {
	sock := socketPath(t)
	ln := listen(t, sock)
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = bufio.NewReader(c).ReadString('\n')
		_, _ = io.WriteString(c, "OK 7\n")
		time.Sleep(600 * time.Millisecond) // past the call's deadline
		_, _ = io.WriteString(c, "x")
		_, _ = io.Copy(io.Discard, c)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	conn, err := vsockdial.Dial(ctx, sock, 1024)
	require.NoError(t, err)
	defer conn.Close()

	one := make([]byte, 1)
	_, err = io.ReadFull(conn, one)
	require.NoError(t, err, "the stream outlives the deadline of the call that opened it")
	assert.Equal(t, "x", string(one))
}
