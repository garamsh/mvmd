package firecracker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// maxHandshakeLine bounds the answer to CONNECT; Firecracker's is "OK <port>\n"
// with the guest's port number.
const maxHandshakeLine = 64

// acknowledged reports whether the line is "OK <n>\n": Firecracker answers
// with the host-side port it assigned the connection, not the guest's port
// (docs/vsock.md).
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

// GuestConn implements machine.Driver: it connects to the vsock socket in the
// run directory, asks for the guest's port and reads Firecracker's answer
// before any byte of the stream behind it.
func (d *Driver) GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	sock, err := d.socket(id, vsockSocketName)
	if err != nil {
		return nil, err
	}
	conn, err := unixsock.DialContext(ctx, sock)
	if err != nil {
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
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
