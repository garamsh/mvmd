// Package vsockdial opens a stream to a guest port through the hybrid vsock
// socket a VMM serves on the host: Firecracker's and Cloud Hypervisor's both
// take "CONNECT <port>\n" and answer "OK <n>\n" before the stream begins.
package vsockdial

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// maxHandshakeLine bounds the answer to CONNECT, "OK <n>\n".
const maxHandshakeLine = 64

// acknowledged reports whether the line is "OK <n>\n": the VMM answers with
// the host-side port it assigned the connection, not the guest's port
// (Firecracker's and Cloud Hypervisor's docs/vsock.md).
func acknowledged(line []byte) bool {
	digits, ok := bytes.CutPrefix(line, []byte("OK "))
	digits, ok2 := bytes.CutSuffix(digits, []byte("\n"))
	if !ok || !ok2 || len(digits) == 0 {
		return false
	}
	for _, c := range digits {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Dial connects to the vsock socket at the path, asks for the guest's port and
// reads the VMM's answer before any byte of the stream behind it. Every
// failure wraps machine.ErrGuestUnreachable. When the socket is absent or
// refused, the VMM has ended, and the error also matches machine.ErrVMMEnded.
func Dial(ctx context.Context, socket string, port uint32) (net.Conn, error) {
	conn, err := unixsock.DialContext(ctx, socket)
	if err != nil {
		return nil, connectErr(err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	// A cancelled ctx ends a handshake that has no deadline.
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	fail := func(err error) (net.Conn, error) {
		_ = conn.Close()
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		return fail(err)
	}
	// One byte at a time: nothing of the stream behind the line is consumed.
	var line []byte
	one := make([]byte, 1)
	for len(line) == 0 || line[len(line)-1] != '\n' {
		if len(line) == maxHandshakeLine {
			return fail(fmt.Errorf("no end of line in %d bytes of the answer", maxHandshakeLine))
		}
		if _, err := conn.Read(one); err != nil {
			return fail(err)
		}
		line = append(line, one[0])
	}
	if !acknowledged(line) {
		return fail(fmt.Errorf("the host side answered %q", line))
	}
	if !stop() {
		return fail(ctx.Err())
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// connectErr reports whether the dial error indicates the VMM has ended:
// the socket file is absent (ENOENT) or a socket file exists but nobody
// listens on it (ECONNREFUSED). In those cases the error matches both
// ErrVMMEnded and ErrGuestUnreachable; otherwise only ErrGuestUnreachable.
func connectErr(err error) error {
	var netErr net.Error
	if errors.As(err, &netErr) {
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			var errno unix.Errno
			if errors.As(opErr.Unwrap(), &errno) {
				if errno == unix.ENOENT || errno == unix.ECONNREFUSED {
					return errors.Join(machine.ErrGuestUnreachable, machine.ErrVMMEnded, err)
				}
			}
		}
	}
	return errors.Join(machine.ErrGuestUnreachable, err)
}
