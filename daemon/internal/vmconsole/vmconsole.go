// Package vmconsole reaches a VMM's serial console as the supervisor leaves it:
// a live session on the socket the supervisor serves beside the run directory
// (ADR 0020), and the log it captured in the run directory. Both VMMs' serial
// devices are their standard output and input, which the supervisor connects
// to that socket and that log.
package vmconsole

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"syscall"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// Dial opens a session on the console socket of the Machine whose run
// directory it is. The socket is the supervisor's, in the state directory the
// VMM cannot reach, so its path is not read from the run directory. A socket
// that is gone or refuses is a VMM that has ended.
func Dial(ctx context.Context, runDir string) (io.ReadWriteCloser, error) {
	path := statedir.ConsoleSocket(statedir.Of(runDir))
	conn, err := unixsock.DialContext(ctx, path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
			return nil, fmt.Errorf("%w: %v", machine.ErrConsoleUnavailable, err)
		}
		return nil, fmt.Errorf("open the console: %w", err)
	}
	return conn, nil
}

// limited reads at most what the log held when it was opened.
type limited struct {
	io.Reader
	io.Closer
}

// OpenLog opens what the supervisor captured of the VMM's standard output in
// the run directory. The directory belongs to the VMM's user, so the file is
// opened without following a link and refused unless it is a regular file, and
// it is read up to its size at the call.
func OpenLog(ctx context.Context, runDir string) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path := statedir.VMMLog(runDir)
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("%w: %v", machine.ErrConsoleUnavailable, err)
		}
		return nil, fmt.Errorf("open the console log: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("read the console log: %w", err)
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s is not a regular file", machine.ErrConsoleUnavailable, path)
	}
	return limited{io.LimitReader(f, info.Size()), f}, nil
}
