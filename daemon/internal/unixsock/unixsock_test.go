package unixsock_test

import (
	"context"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// maxAddress is what a Unix socket address holds.
const maxAddress = 107

// longDir makes a directory whose path leaves less than the name of a socket
// under the 107 bytes, which the plain form of an address cannot hold.
func longDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for len(dir) <= maxAddress {
		dir = filepath.Join(dir, strings.Repeat("d", 40))
	}
	require.NoError(t, os.MkdirAll(dir, 0o755))
	return dir
}

// serve answers each connection with one byte and ends it.
func serve(t *testing.T, l net.Listener, b byte) {
	t.Helper()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_, _ = c.Write([]byte{b})
			_ = c.Close()
		}
	}()
}

func readByte(t *testing.T, c net.Conn) byte {
	t.Helper()
	defer c.Close()
	got, err := io.ReadAll(c)
	require.NoError(t, err)
	require.Len(t, got, 1)
	return got[0]
}

func TestListenAndDial_ExchangeAByteAtAPathOverTheAddressLimit(t *testing.T) {
	sock := filepath.Join(longDir(t), "api.sock")
	require.Greater(t, len(sock), maxAddress)

	l, err := unixsock.Listen(sock)
	require.NoError(t, err)
	defer l.Close()
	serve(t, l, 'k')

	c, err := unixsock.DialContext(context.Background(), sock)
	require.NoError(t, err)
	assert.Equal(t, byte('k'), readByte(t, c))
	assert.Equal(t, sock, l.Addr().String())
}

func TestPlainPath_FailsAtThePathTheDescriptorFormServes(t *testing.T) {
	sock := filepath.Join(longDir(t), "api.sock")

	_, err := net.Listen("unix", sock)
	assert.Error(t, err, "the control: the full path is over what an address holds")
	_, err = net.Dial("unix", sock)
	assert.Error(t, err)

	l, err := unixsock.Listen(sock)
	require.NoError(t, err)
	l.Close()
}

func TestDir_DialReachesTheDirectoryThatWasOpened(t *testing.T) {
	parent := longDir(t)
	opened := filepath.Join(parent, "state")
	require.NoError(t, os.Mkdir(opened, 0o755))
	l, err := unixsock.Listen(filepath.Join(opened, "s.sock"))
	require.NoError(t, err)
	defer l.Close()
	serve(t, l, 'o')

	dir, err := unixsock.Open(opened)
	require.NoError(t, err)
	defer dir.Close()

	// The path now names another directory with a listener of its own.
	require.NoError(t, os.Rename(opened, opened+".old"))
	require.NoError(t, os.Mkdir(opened, 0o755))
	other, err := unixsock.Listen(filepath.Join(opened, "s.sock"))
	require.NoError(t, err)
	defer other.Close()
	serve(t, other, 'n')

	c, err := dir.DialContext(context.Background(), "s.sock")
	require.NoError(t, err)
	assert.Equal(t, byte('o'), readByte(t, c), "the directory that was opened")

	c, err = unixsock.DialContext(context.Background(), filepath.Join(opened, "s.sock"))
	require.NoError(t, err)
	assert.Equal(t, byte('n'), readByte(t, c), "a path is read again at each call")
}

func TestListener_CloseRemovesTheSocket(t *testing.T) {
	sock := filepath.Join(longDir(t), "api.sock")
	l, err := unixsock.Listen(sock)
	require.NoError(t, err)
	require.NoError(t, l.Close())

	_, err = os.Lstat(sock)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.NoError(t, l.Close(), "a second Close is not an error")
}

func TestDialContext_NamesTheSocketAndKeepsTheCause(t *testing.T) {
	dir := longDir(t)
	sock := filepath.Join(dir, "gone.sock")

	_, err := unixsock.DialContext(context.Background(), sock)
	require.Error(t, err)
	assert.ErrorIs(t, err, fs.ErrNotExist)
	assert.Contains(t, err.Error(), sock)
	assert.NotContains(t, err.Error(), "/proc/")

	_, err = unixsock.DialContext(context.Background(), filepath.Join(dir, "no", "dir.sock"))
	assert.ErrorIs(t, err, fs.ErrNotExist)
}

func TestOpen_RefusesALinkAndAFile(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	require.NoError(t, os.Symlink(dir, link))
	file := filepath.Join(dir, "file")
	require.NoError(t, os.WriteFile(file, nil, 0o600))

	d, err := unixsock.Open(dir)
	require.NoError(t, err, "the directory the link points at opens, so the refusals below are not Open refusing everything")
	require.NoError(t, d.Close())

	_, err = unixsock.Open(link)
	assert.Error(t, err)
	_, err = unixsock.Open(file)
	assert.Error(t, err)
}

func TestAwait_ReturnsOnceTheSocketAcceptsAndGivesUpWhenTheContextEnds(t *testing.T) {
	sock := filepath.Join(longDir(t), "late.sock")

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := unixsock.Await(ctx, sock)
	require.ErrorIs(t, err, context.DeadlineExceeded, "no one listens: the wait ends with its context")
	assert.Contains(t, err.Error(), sock)

	bound := make(chan net.Listener, 1)
	go func() {
		time.Sleep(100 * time.Millisecond)
		l, err := unixsock.Listen(sock)
		if err != nil {
			close(bound)
			return
		}
		bound <- l
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	ctx, cancel = context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	require.NoError(t, unixsock.Await(ctx, sock), "a socket bound after the wait began is found")
	if l := <-bound; l != nil {
		_ = l.Close()
	}
}
