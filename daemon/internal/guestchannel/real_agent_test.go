package guestchannel_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests drive the adapter against the real Guest Agent, the host-side
// binary guest-agent-channel, over a Unix socket. `make test` builds the binary
// and names it in GUEST_AGENT_CHANNEL; a run without it fails, since a pass
// that never reached the agent would say nothing about the adapter.

// socketConnector is the dialer a Firecracker Driver has for the host side of
// a guest's vsock: it connects to the Unix socket, writes `CONNECT <port>`,
// and reads the `OK <port>` line before any byte of the gRPC stream.
type socketConnector struct{ path string }

func (c socketConnector) GuestConn(ctx context.Context, _ machine.MachineID, port uint32) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", c.path)
	if err != nil {
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	if _, err := fmt.Fprintf(conn, "CONNECT %d\n", port); err != nil {
		_ = conn.Close()
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	// One byte at a time: nothing of the stream behind the line is consumed.
	var line []byte
	one := make([]byte, 1)
	for len(line) == 0 || line[len(line)-1] != '\n' {
		if _, err := conn.Read(one); err != nil {
			_ = conn.Close()
			return nil, errors.Join(machine.ErrGuestUnreachable, err)
		}
		line = append(line, one[0])
	}
	if want := fmt.Sprintf("OK %d\n", port); string(line) != want {
		_ = conn.Close()
		return nil, fmt.Errorf("%w: the host side answered %q", machine.ErrGuestUnreachable, line)
	}
	return conn, nil
}

// realAgent starts the binary on a socket in a temporary directory, with the
// handshake for the Guest Agent's port, and returns the adapter over it.
func realAgent(t *testing.T) (*guestchannel.Guest, string) {
	t.Helper()
	bin := os.Getenv("GUEST_AGENT_CHANNEL")
	if bin == "" {
		t.Fatal("GUEST_AGENT_CHANNEL names no binary: run these tests through `make test`, which builds guest-agent-channel and sets it")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("the Guest Agent binary %s is absent (%v): run these tests through `make test`, which builds it", bin, err)
	}
	dir, err := os.MkdirTemp("", "agent")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	socket := filepath.Join(dir, "a.sock")
	cmd := exec.Command(bin, "--handshake-port", fmt.Sprint(machine.GuestAgentPort), socket)
	cmd.Stderr = os.Stderr
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	})
	// The socket file exists from bind(), before listen(): a dial is accepted
	// only after that, so the wait is a dial that succeeds.
	require.Eventually(t, func() bool {
		conn, err := net.Dial("unix", socket)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 10*time.Second, 10*time.Millisecond, "the binary never accepted a connection")
	return guestchannel.NewGuest(socketConnector{socket}), socket
}

// readOutput reads an exec's output from the offset to its record.
func readOutput(g *guestchannel.Guest, exec machine.ExecID, after uint64) ([]machine.OutputChunk, machine.ExecRecord, error) {
	var chunks []machine.OutputChunk
	rec, err := g.ReadExecOutput(context.Background(), "m", exec, after, func(c machine.OutputChunk) error {
		chunks = append(chunks, machine.OutputChunk{Stream: c.Stream, Offset: c.Offset, Data: append([]byte(nil), c.Data...)})
		return nil
	})
	return chunks, rec, err
}

// plainConnector opens the socket and sends no handshake.
type plainConnector struct{ path string }

func (c plainConnector) GuestConn(ctx context.Context, _ machine.MachineID, _ uint32) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", c.path)
}

func TestRealAgent_HealthAndTheHandshake(t *testing.T) {
	g, socket := realAgent(t)
	require.NoError(t, g.Health(context.Background(), "m"))

	// Control: a connector that sends no handshake gets no agent, since the
	// binary closes a connection that opens with anything but CONNECT.
	err := guestchannel.NewGuest(plainConnector{socket}).Health(context.Background(), "m")
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
}

// guest-agent-channel is not the guest's init, so it refuses Terminate as a
// failed precondition: the agent answered, so the port's error is not an
// unreachable agent. The controls are Health on the same connection route,
// which the agent serves, and a connector that opens nothing, which is
// unreachable.
func TestRealAgent_TerminateIsRefusedAsAnAnswer(t *testing.T) {
	ctx := context.Background()
	g, socket := realAgent(t)

	err := g.Terminate(ctx, "m")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "FailedPrecondition")
	assert.NotErrorIs(t, err, machine.ErrGuestUnreachable, "the agent answered")
	require.NoError(t, g.Health(ctx, "m"), "control: the same agent answers a call it serves")

	err = guestchannel.NewGuest(plainConnector{socket}).Terminate(ctx, "m")
	require.ErrorIs(t, err, machine.ErrGuestUnreachable, "control: an agent that does not answer the handshake is unreachable")
}

func TestRealAgent_ExecAndItsOutput(t *testing.T) {
	ctx := context.Background()
	g, _ := realAgent(t)
	marks := filepath.Join(t.TempDir(), "marks")
	spec := machine.ExecSpec{Command: []string{"/bin/sh", "-c", `printf hello; printf oops >&2; echo run >> "$MARKS"; exit 3`}, Env: map[string]string{"MARKS": marks}}

	rec, err := g.Exec(ctx, "m", "x", spec)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecID("x"), rec.ID)
	assert.Contains(t, []machine.ExecStatus{machine.ExecStarted, machine.ExecExited}, rec.Status)

	// The output is read by offset, ends with the record, and the exit code is
	// the command's.
	var chunks []machine.OutputChunk
	require.Eventually(t, func() bool {
		var final machine.ExecRecord
		chunks, final, err = readOutput(g, "x", 0)
		return err == nil && final.Status == machine.ExecExited
	}, 10*time.Second, 20*time.Millisecond)
	var out, errOut []byte
	for _, c := range chunks {
		if c.Stream == machine.OutputStdout {
			out = append(out, c.Data...)
		} else {
			errOut = append(errOut, c.Data...)
		}
	}
	assert.Equal(t, "hello", string(out))
	assert.Equal(t, "oops", string(errOut))
	_, final, err := readOutput(g, "x", 0)
	require.NoError(t, err)
	assert.Equal(t, int32(3), final.ExitCode)
	assert.Zero(t, final.TermSignal)

	// Reading after the end of the output returns the record and no bytes.
	tail, _, err := readOutput(g, "x", uint64(len("hello")+len("oops")))
	require.NoError(t, err)
	assert.Empty(t, tail)

	// A repeated exec ID returns the record and runs the command once.
	again, err := g.Exec(ctx, "m", "x", spec)
	require.NoError(t, err)
	assert.Equal(t, machine.ExecID("x"), again.ID)
	marked, err := os.ReadFile(marks)
	require.NoError(t, err)
	assert.Equal(t, "run\n", string(marked), "the command ran once")

	// Control: another ID runs it again.
	_, err = g.Exec(ctx, "m", "y", spec)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		b, _ := os.ReadFile(marks)
		return string(b) == "run\nrun\n"
	}, 10*time.Second, 20*time.Millisecond)
}

// trackingConnector records the connections it opens, so a test can reset one
// as a restore does.
type trackingConnector struct {
	socketConnector
	mu    sync.Mutex
	conns []net.Conn
}

func (c *trackingConnector) GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	conn, err := c.socketConnector.GuestConn(ctx, id, port)
	if err == nil {
		c.mu.Lock()
		c.conns = append(c.conns, conn)
		c.mu.Unlock()
	}
	return conn, err
}

func (c *trackingConnector) opened() []net.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]net.Conn(nil), c.conns...)
}

// A connection reset in the middle of an output stream is replaced, and the
// read resumes after the last byte it passed on: the caller sees every byte
// once.
func TestRealAgent_OutputResumesByOffsetAfterAForcedReconnect(t *testing.T) {
	ctx := context.Background()
	_, socket := realAgent(t)
	conn := &trackingConnector{socketConnector: socketConnector{socket}}
	g := guestchannel.NewGuest(conn)
	_, err := g.Exec(ctx, "m", "slow", machine.ExecSpec{Command: []string{"/bin/sh", "-c", "printf aaaa; sleep 1; printf bbbb"}})
	require.NoError(t, err)

	var got []byte
	first := make(chan struct{})
	type result struct {
		rec machine.ExecRecord
		err error
	}
	done := make(chan result, 1)
	go func() {
		rec, err := g.ReadExecOutput(ctx, "m", "slow", 0, func(c machine.OutputChunk) error {
			assert.Equal(t, uint64(len(got)), c.Offset, "no gap and no repeat")
			got = append(got, c.Data...)
			if len(got) == 4 {
				close(first)
			}
			return nil
		})
		done <- result{rec, err}
	}()
	select {
	case <-first:
	case <-time.After(10 * time.Second):
		t.Fatal("the first output never arrived")
	}
	opened := conn.opened()
	require.Len(t, opened, 2, "the setup: one connection for the exec and one for the read")
	require.NoError(t, opened[1].Close())

	select {
	case r := <-done:
		require.NoError(t, r.err)
		assert.Equal(t, machine.ExecExited, r.rec.Status)
	case <-time.After(15 * time.Second):
		t.Fatal("the read never ended")
	}
	assert.Equal(t, "aaaabbbb", string(got))
	assert.Len(t, conn.opened(), 3, "the read was repeated on a new connection")
}

func TestRealAgent_FileCalls(t *testing.T) {
	ctx := context.Background()
	g, _ := realAgent(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	content := strings.Repeat("0123456789abcdef", 20000) // several chunks

	require.NoError(t, g.WriteFile(ctx, "m", path, strings.NewReader(content)))
	var got bytes.Buffer
	require.NoError(t, g.ReadFile(ctx, "m", path, func(c []byte) error { got.Write(c); return nil }))
	assert.Equal(t, content, got.String())

	// A patch applies at the base digest, and a repeat is already applied.
	digest := func(s string) string { sum := sha256.Sum256([]byte(s)); return "sha256:" + hex.EncodeToString(sum[:]) }
	require.NoError(t, g.WriteFile(ctx, "m", path, strings.NewReader("one\ntwo\n")))
	patch := "--- a\n+++ b\n@@ -1,2 +1,2 @@\n one\n-two\n+three\n"
	outcome, err := g.PatchFile(ctx, "m", path, digest("one\ntwo\n"), digest("one\nthree\n"), []byte(patch))
	require.NoError(t, err)
	assert.Equal(t, machine.PatchApplied, outcome)
	outcome, err = g.PatchFile(ctx, "m", path, digest("one\ntwo\n"), digest("one\nthree\n"), []byte(patch))
	require.NoError(t, err)
	assert.Equal(t, machine.PatchAlreadyApplied, outcome)
	disk, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "one\nthree\n", string(disk))

	// Search finds files by pattern, relative to the directory.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "g.log"), nil, 0o600))
	found, err := g.SearchFiles(ctx, "m", dir, "*.log")
	require.NoError(t, err)
	assert.Equal(t, []string{filepath.Join(dir, "g.log")}, found)
}

// Each status the real agent sends maps to the port's error, and each has a
// control: the call that succeeds, or the same call that fails another way.
func TestRealAgent_StatusesMapToThePortsErrors(t *testing.T) {
	ctx := context.Background()
	g, _ := realAgent(t)
	dir := t.TempDir()
	file := filepath.Join(dir, "f")
	require.NoError(t, os.WriteFile(file, []byte("abc"), 0o600))
	read := func(path string) error {
		return g.ReadFile(ctx, "m", path, func([]byte) error { return nil })
	}

	t.Run("WrongFileKind: a directory read as a file is not a regular file", func(t *testing.T) {
		require.ErrorIs(t, read(dir), machine.ErrNotRegular)
		require.NoError(t, read(file), "control: a regular file reads")
		require.NotErrorIs(t, read(filepath.Join(dir, "absent")), machine.ErrNotRegular, "control: an absent path is another error")
	})

	t.Run("NOT_FOUND by call: a missing path and a missing exec", func(t *testing.T) {
		require.ErrorIs(t, read(filepath.Join(dir, "absent")), machine.ErrFileAbsent)
		_, _, err := readOutput(g, "never-started", 0)
		require.ErrorIs(t, err, machine.ErrExecAbsent)
		require.NotErrorIs(t, err, machine.ErrFileAbsent)
	})

	t.Run("a patch whose file is at neither digest", func(t *testing.T) {
		_, err := g.PatchFile(ctx, "m", file, "sha256:"+strings.Repeat("1", 64), "sha256:"+strings.Repeat("2", 64), []byte("p"))
		require.ErrorIs(t, err, machine.ErrPatchPrecondition)
	})

	t.Run("NotBooted: Boot is refused and is not a failed boot step", func(t *testing.T) {
		err := g.Boot(ctx, "m", machine.GuestBoot{
			ImageUUID: "11111111-1111-1111-1111-111111111111", RootDiskUUID: "22222222-2222-2222-2222-222222222222",
			Run: machine.ExecSpec{Command: []string{"/bin/true"}},
		})
		require.Error(t, err)
		var step *machine.BootStepError
		assert.False(t, errors.As(err, &step), "this agent boots nothing, so no step failed")
		assert.NotErrorIs(t, err, machine.ErrGuestUnreachable, "the agent answered")
	})

	t.Run("ExecOutputGone: a read before the output the agent still holds", func(t *testing.T) {
		// 20 MB is more than the 16 MiB the agent keeps of a command's output.
		_, err := g.Exec(ctx, "m", "big", machine.ExecSpec{Command: []string{"/usr/bin/head", "-c", "20000000", "/dev/zero"}})
		require.NoError(t, err)
		// The window the agent holds moves while the command writes, so wait
		// until the exec is reported exited: a read that follows to the end
		// succeeds only then, and each refusal moves it to the earliest held.
		var gone *machine.OutputGoneError
		var rec machine.ExecRecord
		from := uint64(0)
		require.Eventually(t, func() bool {
			_, rec, err = readOutput(g, "big", from)
			if errors.As(err, &gone) {
				from = gone.Earliest
				return false
			}
			return err == nil && rec.Status == machine.ExecExited
		}, 30*time.Second, 10*time.Millisecond, "the exec was never reported exited; last error: %v", err)

		_, _, err = readOutput(g, "big", 0)
		require.ErrorAs(t, err, &gone, "a read from the start was not refused")
		assert.Positive(t, gone.Earliest)

		// Control: a read from the earliest offset held succeeds, to the end.
		var chunks []machine.OutputChunk
		chunks, rec, err = readOutput(g, "big", gone.Earliest)
		require.NoError(t, err)
		assert.Equal(t, machine.ExecExited, rec.Status)
		require.NotEmpty(t, chunks)
		assert.Equal(t, gone.Earliest, chunks[0].Offset)
	})
}
