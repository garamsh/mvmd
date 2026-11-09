package firecracker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Console implements machine.Driver. The Driver does not report
// machine.CapabilityConsole, so the lifecycle refuses the call as unsupported
// before it gets here (#91). Firecracker's serial device is the VMM's
// standard input and output, and the supervisor gives the VMM no standard input
// that reaches a session, so a session that writes to the guest's console
// cannot be opened yet: it is unavailable, and nothing is faked.
func (d *Driver) Console(context.Context, machine.MachineID) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("%w: Firecracker's serial input is the VMM's standard input, which the supervisor does not connect to a session", machine.ErrConsoleUnavailable)
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
	path := filepath.Join(dir, consoleLogName)
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
