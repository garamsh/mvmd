package supervisor

import (
	"errors"
	"fmt"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

// terminal is the controlling side of the pseudo-terminal whose subordinate
// side is the VMM's standard input and output (ADR 0025). It reads what the VMM
// writes and writes what the VMM reads.
type terminal struct {
	master *os.File
}

// openTerminal opens a pseudo-terminal pair, sets it to raw mode and opens its
// subordinate side twice, once for the VMM's standard input and once for its
// standard output. They are two open file descriptions, so a VMM that makes its
// input non-blocking does not make its output so too. No open takes the
// terminal as a controlling terminal of the caller.
//
// The caller closes in and out once the VMM holds its own: until then the
// terminal's output does not end.
func openTerminal() (t *terminal, in, out *os.File, err error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open /dev/ptmx: %w", err)
	}
	master := os.NewFile(uintptr(fd), "/dev/ptmx")
	defer func() {
		if err != nil {
			master.Close()
			if in != nil {
				in.Close()
			}
			in, out = nil, nil
		}
	}()
	if err := unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0); err != nil {
		return nil, nil, nil, fmt.Errorf("unlock the pseudo-terminal: %w", err)
	}
	n, err := unix.IoctlGetInt(fd, unix.TIOCGPTN)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("find the pseudo-terminal's subordinate side: %w", err)
	}
	path := fmt.Sprintf("/dev/pts/%d", n)
	if in, err = openSubordinate(path); err != nil {
		return nil, nil, nil, err
	}
	if out, err = openSubordinate(path); err != nil {
		return nil, nil, nil, err
	}
	if err := makeRaw(fd); err != nil {
		return nil, nil, nil, fmt.Errorf("set the pseudo-terminal to raw mode: %w", err)
	}
	return &terminal{master: master}, in, out, nil
}

func openSubordinate(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	return os.NewFile(uintptr(fd), path), nil
}

// makeRaw is cfmakeraw on the terminal: no echo, no translation of input or
// output, no signals and no line buffering, so a byte passes unchanged and the
// guest's own line discipline does the echo.
func makeRaw(fd int) error {
	t, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	if err != nil {
		return err
	}
	t.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP | unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	t.Oflag &^= unix.OPOST
	t.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	t.Cflag &^= unix.CSIZE | unix.PARENB
	t.Cflag |= unix.CS8
	t.Cc[unix.VMIN] = 1
	t.Cc[unix.VTIME] = 0
	return unix.IoctlSetTermios(fd, unix.TCSETS, t)
}

// Read reads what the VMM wrote. The controlling side reports EIO once the last
// subordinate descriptor has closed and what was written is read, which is the
// end of the output.
func (t *terminal) Read(p []byte) (int, error) {
	n, err := t.master.Read(p)
	if errors.Is(err, unix.EIO) {
		err = io.EOF
	}
	return n, err
}

// Write writes what the VMM is to read.
func (t *terminal) Write(p []byte) (int, error) { return t.master.Write(p) }

// Close closes the controlling side, which ends a Read waiting on it.
func (t *terminal) Close() error { return t.master.Close() }
