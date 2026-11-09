package firecracker_test

import (
	"bufio"
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The handshake is vsockdial's, and its tests hold it. These hold the wiring:
// the socket is the run directory's vsock.sock, and the port is the caller's.

// fakeVsock serves the host side of a guest's vsock: it reads the connect
// request and answers with reply, then writes behind it. It records the
// request line it read.
func fakeVsock(t *testing.T, path, reply, behind string) <-chan string {
	t.Helper()
	ln := listenUnix(t, path)
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

func TestGuestConn_AsksTheRunDirectorysVsockSocketForThePort(t *testing.T) {
	n := newNode(t)
	got := fakeVsock(t, n.vsockSocket(), "OK 1073741824\n", "stream")

	conn, err := n.driver.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.NoError(t, err)
	defer conn.Close()

	assert.Equal(t, "CONNECT 1024\n", <-got)
	buf := make([]byte, 6)
	_, err = io.ReadFull(conn, buf)
	require.NoError(t, err)
	assert.Equal(t, "stream", string(buf))
}

func TestGuestConn_IsUnreachableWhenNoVMMListens(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.GuestConn(context.Background(), "m1", 1024)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
}
