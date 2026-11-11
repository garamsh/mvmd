package app_test

import (
	"context"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// serveConsole makes the console of "m" write the banner and echo its input.
func serveConsole(n *node, banner string) {
	n.app.Substitutes.Driver.ServeConsole("m", func(c io.ReadWriteCloser) {
		defer c.Close()
		_, _ = c.Write([]byte(banner))
		buf := make([]byte, 64)
		for {
			k, err := c.Read(buf)
			if k > 0 {
				_, _ = c.Write(append([]byte("echo:"), buf[:k]...))
			}
			if err != nil {
				return
			}
		}
	})
}

func recvOutput(t *testing.T, s v1alpha2.RuntimeService_ConsoleClient, want string) {
	t.Helper()
	var got []byte
	for len(got) < len(want) {
		r, err := s.Recv()
		require.NoError(t, err)
		got = append(got, r.GetOutput()...)
	}
	assert.Equal(t, want, string(got))
}

// The Console RPC is served: the Machine named in the first message, then
// bytes both ways.
func TestAPI_ConsoleIsServedBothWays(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := running(t)
	serveConsole(n, "login: ")

	s, err := n.client.Console(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&v1alpha2.ConsoleRequest{MachineId: "m"}))
	recvOutput(t, s, "login: ")
	require.NoError(t, s.Send(&v1alpha2.ConsoleRequest{Input: []byte("root")}))
	recvOutput(t, s, "echo:root")

	require.NoError(t, s.CloseSend())
	_, err = s.Recv()
	assert.Equal(t, io.EOF, err, "the client ending its side ends the session cleanly")
}

func TestAPI_ConsoleEndsCleanlyWhenTheConsoleDoes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := running(t)
	n.app.Substitutes.Driver.ServeConsole("m", func(c io.ReadWriteCloser) {
		_, _ = c.Write([]byte("bye"))
		_ = c.Close()
	})
	s, err := n.client.Console(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&v1alpha2.ConsoleRequest{MachineId: "m"}))
	recvOutput(t, s, "bye")
	_, err = s.Recv()
	assert.Equal(t, io.EOF, err)
}

// serveRecording makes the console of the Machine record every byte written to
// it, and returns what it has recorded so far.
func serveRecording(n *node, id string) func() string {
	var mu sync.Mutex
	var got []byte
	n.app.Substitutes.Driver.ServeConsole(machine.MachineID(id), func(c io.ReadWriteCloser) {
		defer c.Close()
		buf := make([]byte, 64)
		for {
			k, err := c.Read(buf)
			mu.Lock()
			got = append(got, buf[:k]...)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	})
	return func() string {
		mu.Lock()
		defer mu.Unlock()
		return string(got)
	}
}

// A later message that names a Machine is ignored: the session stays on the
// Machine the first message named (runtime-api.md §Guest access).
func TestAPI_ConsoleIgnoresAMachineNamedByALaterMessage(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := running(t)
	serveConsole(n, "login: ")
	_, err := n.client.CreateMachine(ctx, create("b"))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "b"})
	require.NoError(t, err)
	recorded := serveRecording(n, "b")

	// Control: input sent in a session on "b" is visible in "b"'s console.
	control, err := n.client.Console(ctx)
	require.NoError(t, err)
	require.NoError(t, control.Send(&v1alpha2.ConsoleRequest{MachineId: "b", Input: []byte("to-b")}))
	require.NoError(t, control.CloseSend())
	_, err = control.Recv()
	require.Equal(t, io.EOF, err)
	require.Equal(t, "to-b", recorded())

	s, err := n.client.Console(ctx)
	require.NoError(t, err)
	require.NoError(t, s.Send(&v1alpha2.ConsoleRequest{MachineId: "m"}))
	recvOutput(t, s, "login: ")
	require.NoError(t, s.Send(&v1alpha2.ConsoleRequest{MachineId: "b", Input: []byte("later")}))
	recvOutput(t, s, "echo:later")
	require.NoError(t, s.CloseSend())
	_, err = s.Recv()
	require.Equal(t, io.EOF, err)
	assert.Equal(t, "to-b", recorded(), "the input did not reach the Machine the later message named")
}

// Refusals are the runtime API's outcomes.
func TestAPI_ConsoleRefusals(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n := running(t)
	serveConsole(n, "x")
	_, err := n.client.CreateMachine(ctx, create("idle"))
	require.NoError(t, err)

	open := func(first *v1alpha2.ConsoleRequest) error {
		s, err := n.client.Console(ctx)
		require.NoError(t, err)
		if first != nil {
			require.NoError(t, s.Send(first))
		} else {
			require.NoError(t, s.CloseSend())
		}
		_, err = s.Recv()
		return err
	}
	t.Run("a Machine that is not Running or Paused", func(t *testing.T) {
		err := open(&v1alpha2.ConsoleRequest{MachineId: "idle"})
		requireKind(t, err, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED)
	})
	t.Run("an absent Machine", func(t *testing.T) {
		requireKind(t, open(&v1alpha2.ConsoleRequest{MachineId: "nope"}), v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND)
	})
	t.Run("a first message that names no Machine", func(t *testing.T) {
		requireKind(t, open(&v1alpha2.ConsoleRequest{Input: []byte("x")}), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED)
	})
	t.Run("an empty stream", func(t *testing.T) {
		requireKind(t, open(nil), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED)
	})
}
