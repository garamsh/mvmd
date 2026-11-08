package app

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/user"
	"strconv"
	"sync"
	"syscall"
)

// umaskMu serializes the process-wide umask change around creating a socket.
var umaskMu sync.Mutex

// listen creates the Unix socket at the configured path with the configured
// mode and group. The socket is created with no permission for anyone and
// opened up after its group is set, so no client reaches it with a looser
// permission than configured, even for an instant.
func listen(c SocketConfig) (net.Listener, error) {
	mode, err := c.perm()
	if err != nil {
		return nil, err
	}
	gid := -1
	if c.Group != "" {
		g, err := user.LookupGroup(c.Group)
		if err != nil {
			return nil, fmt.Errorf("socket.group: %w", err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, fmt.Errorf("socket.group %q has the group ID %q", c.Group, g.Gid)
		}
	}
	// A socket left by a previous run is removed; any other file is not ours.
	if info, err := os.Lstat(c.Path); err == nil {
		if info.Mode().Type() != fs.ModeSocket {
			return nil, fmt.Errorf("%s exists and is not a socket", c.Path)
		}
		if err := os.Remove(c.Path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}

	umaskMu.Lock()
	old := syscall.Umask(0o777)
	l, err := net.Listen("unix", c.Path)
	syscall.Umask(old)
	umaskMu.Unlock()
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", c.Path, err)
	}
	if err := os.Chown(c.Path, -1, gid); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("set the group of %s: %w", c.Path, err)
	}
	if err := os.Chmod(c.Path, mode); err != nil {
		_ = l.Close()
		return nil, fmt.Errorf("set the mode of %s: %w", c.Path, err)
	}
	return l, nil
}
