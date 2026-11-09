package supervisor

import (
	"bytes"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// syncBuffer is a log the console writes while the test reads.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// vmm is a console served on a socket in a temporary directory, with the VMM's
// output written to out and its input read from in.
type vmm struct {
	sock string
	out  *os.File
	in   *os.File
	log  *syncBuffer
	c    *Console
}

func newVMM(t *testing.T) *vmm {
	t.Helper()
	// A short directory: a Unix socket path holds 107 bytes.
	dir, err := os.MkdirTemp("", "cs")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, ConsoleSocketName)
	l, err := listenConsole(sock)
	require.NoError(t, err)
	inR, inW, err := os.Pipe()
	require.NoError(t, err)
	outR, outW, err := os.Pipe()
	require.NoError(t, err)
	v := &vmm{sock: sock, out: outW, in: inR, log: &syncBuffer{}}
	v.c = ServeConsole(l, inW, outR, v.log)
	t.Cleanup(func() { _ = outW.Close(); _ = inR.Close(); _ = inW.Close() })
	return v
}

func (v *vmm) session(t *testing.T) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", v.sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func (v *vmm) write(t *testing.T, s string) {
	t.Helper()
	_, err := v.out.WriteString(s)
	require.NoError(t, err)
}

// read reads n bytes from the session within a second.
func read(t *testing.T, c net.Conn, n int) string {
	t.Helper()
	buf := make([]byte, n)
	require.NoError(t, c.SetReadDeadline(time.Now().Add(time.Second)))
	_, err := io.ReadFull(c, buf)
	require.NoError(t, err)
	return string(buf)
}

func TestListenConsole_IsReadableByItsOwnerAlone(t *testing.T) {
	v := newVMM(t)
	info, err := os.Stat(v.sock)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
}

func TestConsole_TheLogHoldsTheOutputWithNoSessionOpen(t *testing.T) {
	v := newVMM(t)
	v.write(t, "boot\n")
	require.NoError(t, v.out.Close())
	v.c.Wait()
	assert.Equal(t, "boot\n", v.log.String())
}

func TestConsole_ASessionReadsTheOutputAndItsInputReachesTheVMM(t *testing.T) {
	v := newVMM(t)
	s := v.session(t)
	// The session is registered when the console accepts it, which the VMM's
	// output after a round trip through the input proves.
	_, err := s.Write([]byte("ping\n"))
	require.NoError(t, err)
	buf := make([]byte, 5)
	require.NoError(t, v.in.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = io.ReadFull(v.in, buf)
	require.NoError(t, err)
	assert.Equal(t, "ping\n", string(buf), "input reaches the VMM")

	v.write(t, "pong\n")
	assert.Equal(t, "pong\n", read(t, s, 5), "output reaches the session")
	assert.Equal(t, "pong\n", v.log.String(), "and the log, which the session did not take it from")
}

func TestConsole_SessionsDoNotInterfereWithEachOtherOrTheLog(t *testing.T) {
	v := newVMM(t)
	a, b := v.session(t), v.session(t)
	for _, s := range []net.Conn{a, b} { // both are registered once their input has crossed
		_, err := s.Write([]byte("."))
		require.NoError(t, err)
	}
	require.NoError(t, v.in.SetReadDeadline(time.Now().Add(time.Second)))
	_, err := io.ReadFull(v.in, make([]byte, 2))
	require.NoError(t, err)

	v.write(t, "one\n")
	assert.Equal(t, "one\n", read(t, a, 4))
	assert.Equal(t, "one\n", read(t, b, 4), "each session has all of the output")

	require.NoError(t, a.Close())
	v.write(t, "two\n")
	assert.Equal(t, "two\n", read(t, b, 4), "a session that left does not end the other")
	require.Eventually(t, func() bool { return v.log.String() == "one\ntwo\n" }, time.Second, time.Millisecond)
}

func TestConsole_ASessionThatReadsNothingIsDroppedAndHoldsBackNoOneElse(t *testing.T) {
	v := newVMM(t)
	stuck, live := v.session(t), v.session(t)
	for _, s := range []net.Conn{stuck, live} {
		_, err := s.Write([]byte("."))
		require.NoError(t, err)
	}
	require.NoError(t, v.in.SetReadDeadline(time.Now().Add(time.Second)))
	_, err := io.ReadFull(v.in, make([]byte, 2))
	require.NoError(t, err)

	// More than the session's backlog and the socket's buffers hold, written one
	// line at a time so that the live session, which reads each as it comes, is
	// never behind.
	line := strings.Repeat("x", chunkSize-1) + "\n"
	const lines = 3 * sessionBacklog
	for i := 0; i < lines; i++ {
		v.write(t, line)
		require.Equal(t, line, read(t, live, len(line)), "the live session has line %d", i)
	}
	require.NoError(t, v.out.Close())
	v.c.Wait()
	assert.Equal(t, lines*len(line), len(v.log.String()), "the log is complete")

	rest, err := io.ReadAll(stuck)
	require.NoError(t, err, "the dropped session ends with end of file, not a hang")
	assert.Less(t, len(rest), lines*len(line), "control: it was dropped before the end of the output")
}

func TestConsole_EndsItsSessionsWhenTheVMMsOutputEnds(t *testing.T) {
	v := newVMM(t)
	s := v.session(t)
	_, err := s.Write([]byte("."))
	require.NoError(t, err)
	require.NoError(t, v.in.SetReadDeadline(time.Now().Add(time.Second)))
	_, err = io.ReadFull(v.in, make([]byte, 1))
	require.NoError(t, err)

	v.write(t, "last\n")
	require.NoError(t, v.out.Close())
	got, err := io.ReadAll(s)
	require.NoError(t, err)
	assert.Equal(t, "last\n", string(got), "the session reads to end of file after what was written")

	v.c.Wait()
	_, err = net.Dial("unix", v.sock)
	assert.Error(t, err, "and no session opens once the output has ended")
}
