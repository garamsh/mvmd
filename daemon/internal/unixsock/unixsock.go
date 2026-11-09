// Package unixsock binds and connects to a Unix socket through a descriptor of
// its directory, so that no path length bounds it. A socket address holds 107
// bytes, and the full path of a socket under a state directory is as long as
// the state directory and the Machine ID make it. The address
// /proc/self/fd/<n>/<name>, with n a descriptor of the directory, is short
// whatever the directory's path.
//
// A Dir keeps the directory it opened: a path that is replaced afterwards
// leaves the Dir at the directory it opened, not at the new one.
package unixsock

import (
	"context"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// Dir is an open directory that sockets are bound and connected in. It holds a
// descriptor until Close.
type Dir struct {
	path string
	fd   int
}

// Open opens the directory at path, refusing a link in its last element, to
// address sockets in it.
func Open(path string) (*Dir, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open the directory %s: %w", path, err)
	}
	return &Dir{path: path, fd: fd}, nil
}

// Close releases the descriptor. A connection made through the Dir outlives it.
// A listener does not: Listen hands the Dir to its listener, which closes it,
// so the caller closes the Dir only when Listen was not called or failed.
func (d *Dir) Close() error {
	return unix.Close(d.fd)
}

// address is the short address of name in the directory.
func (d *Dir) address(name string) string {
	return "/proc/self/fd/" + strconv.Itoa(d.fd) + "/" + name
}

// named is the address the caller knows the socket by.
func (d *Dir) named(name string) *net.UnixAddr {
	return &net.UnixAddr{Name: filepath.Join(d.path, name), Net: "unix"}
}

// relabel puts the socket's own path in a network error in place of the
// descriptor's address, which tells the reader of the error nothing.
func (d *Dir) relabel(err error, name string) error {
	var op *net.OpError
	if errors.As(err, &op) {
		op.Addr = d.named(name)
	}
	return err
}

// DialContext connects to the socket called name in the directory.
func (d *Dir) DialContext(ctx context.Context, name string) (net.Conn, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", d.address(name))
	if err != nil {
		return nil, d.relabel(err, name)
	}
	return conn, nil
}

// Listen binds the socket called name in the directory. On success the
// listener owns the Dir and closes it with itself.
func (d *Dir) Listen(name string) (net.Listener, error) {
	l, err := net.Listen("unix", d.address(name))
	if err != nil {
		return nil, d.relabel(err, name)
	}
	return &listener{Listener: l, dir: d, addr: d.named(name)}, nil
}

type listener struct {
	net.Listener
	dir  *Dir
	addr net.Addr
	once sync.Once
	err  error
}

func (l *listener) Addr() net.Addr { return l.addr }

// Close closes the listener, which unlinks the socket through the directory's
// descriptor, and then the directory.
func (l *listener) Close() error {
	l.once.Do(func() {
		l.err = errors.Join(l.Listener.Close(), l.dir.Close())
	})
	return l.err
}

// Listen binds the socket at path through a descriptor of its directory.
func Listen(path string) (net.Listener, error) {
	d, err := Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	l, err := d.Listen(filepath.Base(path))
	if err != nil {
		_ = d.Close()
		return nil, err
	}
	return l, nil
}

// DialContext connects to the socket at path through a descriptor of its
// directory.
func DialContext(ctx context.Context, path string) (net.Conn, error) {
	d, err := Open(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	defer d.Close()
	return d.DialContext(ctx, filepath.Base(path))
}

// Await returns once a connection to the socket at path is accepted, which is
// some time after the process that binds it has started, or when ctx ends. It
// closes the connection it made.
func Await(ctx context.Context, path string) error {
	for wait := time.Millisecond; ; wait = min(2*wait, 50*time.Millisecond) {
		conn, err := DialContext(ctx, path)
		if err == nil {
			return conn.Close()
		}
		select {
		case <-ctx.Done():
			return errors.Join(fmt.Errorf("the socket %s never accepted a connection", path), ctx.Err(), err)
		case <-time.After(wait):
		}
	}
}
