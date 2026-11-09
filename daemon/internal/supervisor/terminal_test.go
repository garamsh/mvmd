package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// vmmTerminal is a console opened as the supervisor opens it, with the test
// playing the VMM on the two descriptors the VMM is given.
type vmmTerminal struct {
	dir string
	in  *os.File // the VMM's standard input
	out *os.File // the VMM's standard output
	log *syncBuffer
	c   *Console
}

func openVMMTerminal(t *testing.T) *vmmTerminal {
	t.Helper()
	root, err := os.MkdirTemp("", "ct")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dir := filepath.Join(root, strings.Repeat("d", 100))
	require.NoError(t, os.Mkdir(dir, 0o700))
	v := &vmmTerminal{dir: dir, log: &syncBuffer{}}
	v.c, v.in, v.out, err = OpenConsole(dir, v.log)
	require.NoError(t, err)
	t.Cleanup(func() { _ = v.in.Close(); _ = v.out.Close(); _ = v.c.Close() })
	return v
}

func (v *vmmTerminal) session(t *testing.T) net.Conn {
	t.Helper()
	c, err := unixsock.DialContext(context.Background(), statedir.ConsoleSocket(v.dir))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// readFrom reads n bytes from a descriptor the VMM holds, within a second. The
// descriptor is blocking, so the read runs apart and the wait is on its result.
func readFrom(t *testing.T, f *os.File, n int) string {
	t.Helper()
	type result struct {
		b   []byte
		err error
	}
	got := make(chan result, 1)
	go func() {
		b := make([]byte, n)
		_, err := io.ReadFull(f, b)
		got <- result{b, err}
	}()
	select {
	case r := <-got:
		require.NoError(t, r.err)
		return string(r.b)
	case <-time.After(time.Second):
		t.Fatalf("no %d bytes arrived on %s within a second", n, f.Name())
		return ""
	}
}

func TestOpenConsole_TheTerminalIsRaw(t *testing.T) {
	v := openVMMTerminal(t)
	for _, f := range []*os.File{v.in, v.out} {
		tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
		require.NoError(t, err)
		assert.Zero(t, tio.Lflag&(unix.ECHO|unix.ICANON|unix.ISIG|unix.IEXTEN), "%s: no echo, line buffering or signals", f.Name())
		assert.Zero(t, tio.Oflag&unix.OPOST, "%s: no translation of output", f.Name())
		assert.Zero(t, tio.Iflag&(unix.ICRNL|unix.INLCR|unix.IGNCR|unix.IXON|unix.ISTRIP), "%s: no translation of input", f.Name())
		assert.Equal(t, uint32(unix.CS8), tio.Cflag&unix.CSIZE)
	}
}

func init() {
	cases["terminal session"] = func([]byte) (map[string]string, error) {
		_, in, out, err := openTerminal()
		if err != nil {
			return nil, err
		}
		defer in.Close()
		defer out.Close()
		// A terminal that is some session's controlling terminal has that session.
		_, err = unix.IoctlGetInt(int(in.Fd()), unix.TIOCGSID)
		return map[string]string{"session": errName(err)}, nil
	}
}

// A process that is a session leader with no controlling terminal takes the
// first terminal it opens without O_NOCTTY as its own, and hangs up with it.
func TestOpenConsole_NoProcessTakesTheTerminalAsItsControllingTerminal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), caseEnv+"=terminal session")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	require.NoError(t, cmd.Run(), "the process, a session leader, ran to its end")
	var res map[string]string
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &res))
	assert.Equal(t, "ENOTTY", res["session"], "the terminal has no session")
}

func TestOpenConsole_TheBytesASessionSendsReachTheVMMsInputUnchanged(t *testing.T) {
	v := openVMMTerminal(t)
	s := v.session(t)
	// A carriage return, a line feed, ETX (Ctrl-C) and DEL (the erase character).
	const sent = "ab\r\ncd\n\x03e\x7ff\r"
	_, err := s.Write([]byte(sent))
	require.NoError(t, err)
	assert.Equal(t, sent, readFrom(t, v.in, len(sent)))
}

func TestOpenConsole_TheBytesTheVMMWritesReachTheLogAndASessionUnchangedWithNoEcho(t *testing.T) {
	v := openVMMTerminal(t)
	s := v.session(t)
	// The session is registered once its input has reached the VMM. A terminal
	// that echoed would have sent the input back to the session by then.
	_, err := s.Write([]byte("ping\r"))
	require.NoError(t, err)
	require.Equal(t, "ping\r", readFrom(t, v.in, 5))

	const written = "pong\n\x03\x7f\r"
	_, err = v.out.WriteString(written)
	require.NoError(t, err)
	assert.Equal(t, written, read(t, s, len(written)), "the session reads what the VMM wrote and nothing before it")
	require.Eventually(t, func() bool { return v.log.String() == written }, time.Second, time.Millisecond,
		"the log holds what the VMM wrote and nothing else")
}

func TestOpenConsole_TheVMMsInputAndOutputAreSeparateOpenFileDescriptions(t *testing.T) {
	_, in, out, err := openTerminal()
	require.NoError(t, err)
	defer in.Close()
	defer out.Close()

	flags := func(f *os.File) int {
		var fl int
		var ferr error
		rc, err := f.SyscallConn()
		require.NoError(t, err)
		require.NoError(t, rc.Control(func(fd uintptr) { fl, ferr = unix.FcntlInt(fd, unix.F_GETFL, 0) }))
		require.NoError(t, ferr)
		return fl
	}
	setNonblock := func(f *os.File) {
		rc, err := f.SyscallConn()
		require.NoError(t, err)
		require.NoError(t, rc.Control(func(fd uintptr) { err = unix.SetNonblock(int(fd), true) }))
		require.NoError(t, err)
	}

	require.Zero(t, flags(in)&unix.O_NONBLOCK, "setup: both start blocking")
	require.Zero(t, flags(out)&unix.O_NONBLOCK, "setup: both start blocking")
	setNonblock(in)
	assert.NotZero(t, flags(in)&unix.O_NONBLOCK, "control: the one made non-blocking is")
	assert.Zero(t, flags(out)&unix.O_NONBLOCK, "the other stays blocking")
}

// closedTerminal is a terminal whose VMM wrote output and ended: the last
// subordinate descriptor has closed.
func closedTerminal(t *testing.T, output string) *terminal {
	t.Helper()
	tm, in, out, err := openTerminal()
	require.NoError(t, err)
	_, err = out.WriteString(output)
	require.NoError(t, err)
	require.NoError(t, in.Close())
	require.NoError(t, out.Close())
	t.Cleanup(func() { _ = tm.Close() })
	return tm
}

func TestTerminal_ReadEndsTheOutputWhereTheControllingSideReportsEIO(t *testing.T) {
	_, err := io.ReadAll(closedTerminal(t, "raw\n").master)
	require.ErrorIs(t, err, unix.EIO, "control: the controlling side itself reports EIO after the last subordinate closes")

	got, err := io.ReadAll(closedTerminal(t, "raw\n"))
	require.NoError(t, err, "EIO is the end of the output, not an error")
	assert.Equal(t, "raw\n", string(got), "and what was written before it is read first")
}

func TestOpenConsole_WhenTheVMMEndsTheLogIsCompleteAndTheSessionsEnd(t *testing.T) {
	v := openVMMTerminal(t)
	s := v.session(t)
	_, err := s.Write([]byte("."))
	require.NoError(t, err)
	require.Equal(t, ".", readFrom(t, v.in, 1))

	_, err = v.out.WriteString("last words\n")
	require.NoError(t, err)
	require.NoError(t, v.in.Close())
	require.NoError(t, v.out.Close())

	got, err := io.ReadAll(s)
	require.NoError(t, err)
	assert.Equal(t, "last words\n", string(got), "the session reads to end of file after what was written")
	v.c.Wait()
	assert.Equal(t, "last words\n", v.log.String(), "the log is complete")
}
