package firecracker_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// fakeVsock serves the host side of a guest's vsock: it reads the connect
// request and answers with reply, then writes behind it. It records the
// request line it read.
func fakeVsock(t *testing.T, path, reply, behind string) <-chan string {
	t.Helper()
	ln, err := net.Listen("unix", path)
	require.NoError(t, err)
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

func TestGuestConn_AsksForThePortAndReadsTheAnswerBeforeAnyStreamByte(t *testing.T) {
	n := newNode(t)
	// Firecracker answers with the host-side port it assigned, not the guest's.
	got := fakeVsock(t, n.vsockSocket(), "OK 1073741824\n", "stream")

	conn, err := n.driver.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "CONNECT 1024\n", <-got)
	buf := make([]byte, 6)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "stream", string(buf), "the bytes behind the answer are the stream's, none consumed")
}

func TestGuestConn_IsUnreachableWhenTheHostSideDoesNotAcknowledge(t *testing.T) {
	tests := []struct{ name, reply string }{
		{"the guest refuses and Firecracker closes the connection", ""},
		{"an answer that is not OK", "NO 1024\n"},
		{"an OK without a number", "OK \n"},
		{"an OK with a word after it", "OK port\n"},
		{"an answer with no end of line", "OK 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			fakeVsock(t, n.vsockSocket(), "OK 7\n", "")
			conn, err := n.driver.GuestConn(context.Background(), "m1", 1024)
			require.NoError(t, err, "control: an acknowledged connect succeeds")
			_ = conn.Close()

			n = newNode(t)
			ln, err := net.Listen("unix", n.vsockSocket())
			require.NoError(t, err)
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
			_, err = n.driver.GuestConn(context.Background(), "m1", 1024)
			require.ErrorIs(t, err, machine.ErrGuestUnreachable)
		})
	}
}

func TestGuestConn_IsUnreachableWhenNoVMMListens(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.GuestConn(context.Background(), "m1", 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
}

func TestGuestConn_StopsWaitingForAnAnswerWhenTheContextEnds(t *testing.T) {
	n := newNode(t)
	ln, err := net.Listen("unix", n.vsockSocket())
	require.NoError(t, err)
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
	_, err = n.driver.GuestConn(ctx, "m1", 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	assert.Less(t, time.Since(start), 3*time.Second)
}

func TestGuestConn_LeavesNoDeadlineOnTheStreamItReturns(t *testing.T) {
	n := newNode(t)
	ln, err := net.Listen("unix", n.vsockSocket())
	require.NoError(t, err)
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
	conn, err := n.driver.GuestConn(ctx, "m1", 1024)
	require.NoError(t, err)
	defer conn.Close()

	one := make([]byte, 1)
	_, err = io.ReadFull(conn, one)
	require.NoError(t, err, "the stream outlives the deadline of the call that opened it")
	assert.Equal(t, "x", string(one))
}
