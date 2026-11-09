package firecracker

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

// Console implements machine.Driver: a connection to the console socket the
// supervisor serves beside the run directory (ADR 0020). Firecracker's serial
// device is the VMM's standard input and output, which the supervisor
// connects to that socket. The socket is the supervisor's, in the state
// directory the VMM cannot reach, so the path is not read from the run
// directory. A socket that is gone or refuses is a VMM that has ended.
func (d *Driver) Console(ctx context.Context, id machine.MachineID) (io.ReadWriteCloser, error) {
	run, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	path := statedir.ConsoleSocket(statedir.Of(run))
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

// ConsoleLog implements machine.Driver: what the supervisor captured of the
// VMM's standard output in the run directory, which is the guest's serial
// console since Firecracker's own log goes to firecracker.log. The directory
// belongs to the VMM's user, so the file is opened without following a link and
// refused unless it is a regular file, and it is read up to its size at the
// call.
func (d *Driver) ConsoleLog(ctx context.Context, id machine.MachineID) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	path := statedir.VMMLog(dir)
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
