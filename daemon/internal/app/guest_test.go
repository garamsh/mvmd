package app_test

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// running starts a node with a Running Machine "m".
func running(t *testing.T) *node {
	t.Helper()
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	renew(t, n, 1)
	_, err := n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	return n
}

func execRequest() *v1alpha1.ExecRequest {
	return &v1alpha1.ExecRequest{ExecId: "x", RequestId: "r", MachineId: "m", Command: []string{"echo", "hi"}}
}

// Exec and its output through the wired graph: started once per request ID,
// read by offset, the record at the end carrying the exit code, and a read
// before the held window refused with the earliest offset.
func TestGuestAPI_ExecAndItsOutput(t *testing.T) {
	ctx := context.Background()
	n := running(t)
	g := n.app.Substitutes.Guest
	c := n.client

	resp, err := c.Exec(ctx, execRequest())
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.ExecStatus_EXEC_STATUS_STARTED, resp.GetRecord().GetStatus())
	_, err = c.Exec(ctx, execRequest())
	require.NoError(t, err, "the same request ID")
	assert.Equal(t, 1, g.Calls("exec"), "the command ran once")
	other := execRequest()
	other.RequestId = "other"
	_, err = c.Exec(ctx, other)
	requireKind(t, err, conflict)

	g.Emit("x", machine.OutputStdout, "hello ")
	g.Emit("x", machine.OutputStderr, "world")
	g.Finish("x", 7)

	s, err := c.ReadExecOutput(ctx, &v1alpha1.ReadExecOutputRequest{MachineId: "m", ExecId: "x"})
	require.NoError(t, err)
	first, err := s.Recv()
	require.NoError(t, err)
	assert.Equal(t, "hello ", string(first.GetChunk().GetData()))
	assert.Equal(t, v1alpha1.OutputStream_OUTPUT_STREAM_STDOUT, first.GetChunk().GetStream())
	second, err := s.Recv()
	require.NoError(t, err)
	assert.Equal(t, uint64(6), second.GetChunk().GetOffset())
	end, err := s.Recv()
	require.NoError(t, err)
	assert.Equal(t, int32(7), end.GetRecord().GetExitCode())
	assert.Equal(t, v1alpha1.ExecStatus_EXEC_STATUS_EXITED, end.GetRecord().GetStatus())
	_, err = s.Recv()
	assert.Equal(t, io.EOF, err)

	t.Run("an offset no longer held", func(t *testing.T) {
		g.Retain(8)
		g.Emit("x", machine.OutputStdout, "tail1234")
		bad, err := c.ReadExecOutput(ctx, &v1alpha1.ReadExecOutputRequest{MachineId: "m", ExecId: "x", AfterOffset: 0})
		require.NoError(t, err)
		_, err = bad.Recv()
		requireKind(t, err, precondition)
		earliest, ok := v1alpha1.EarliestOffset(err)
		require.True(t, ok, "the earliest offset held is data a client resumes from")
		assert.Equal(t, uint64(11), earliest)

		// Control: a read from that offset succeeds.
		inside, err := c.ReadExecOutput(ctx, &v1alpha1.ReadExecOutputRequest{MachineId: "m", ExecId: "x", AfterOffset: earliest})
		require.NoError(t, err)
		chunk, err := inside.Recv()
		require.NoError(t, err)
		assert.Equal(t, "tail1234", string(chunk.GetChunk().GetData()))
	})
}

// The files through the wired graph, with the Machine Running; the same calls
// are refused once it is stopped.
func TestGuestAPI_Files(t *testing.T) {
	ctx := context.Background()
	n := running(t)
	g := n.app.Substitutes.Guest
	c := n.client
	g.PutFile("m", "/etc/motd", "old")

	w, err := c.WriteFile(ctx)
	require.NoError(t, err)
	require.NoError(t, w.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Start{Start: &v1alpha1.WriteFileStart{MachineId: "m", Path: "/etc/motd"}}}))
	require.NoError(t, w.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Chunk{Chunk: []byte("new ")}}))
	require.NoError(t, w.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Chunk{Chunk: []byte("content")}}))
	_, err = w.CloseAndRecv()
	require.NoError(t, err)
	got, _ := g.File("m", "/etc/motd")
	assert.Equal(t, "new content", got)

	r, err := c.ReadFile(ctx, &v1alpha1.ReadFileRequest{MachineId: "m", Path: "/etc/motd"})
	require.NoError(t, err)
	var read []byte
	for {
		m, err := r.Recv()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		read = append(read, m.GetChunk()...)
	}
	assert.Equal(t, "new content", string(read))

	_, err = c.PatchFile(ctx, &v1alpha1.PatchFileRequest{MachineId: "m", Path: "/etc/motd",
		BaseDigest: memory.Digest([]byte("new content")), ExpectedDigest: memory.Digest([]byte("patched")), Patch: []byte("patched")})
	require.NoError(t, err)
	_, err = c.PatchFile(ctx, &v1alpha1.PatchFileRequest{MachineId: "m", Path: "/etc/motd",
		BaseDigest: "sha256:0", ExpectedDigest: "sha256:1", Patch: []byte("x")})
	requireKind(t, err, precondition)

	found, err := c.SearchFiles(ctx, &v1alpha1.SearchFilesRequest{MachineId: "m", Path: "/etc/", Pattern: "mot"})
	require.NoError(t, err)
	assert.Equal(t, []string{"/etc/motd"}, found.GetMatches())

	g.PutFile("m", "/etc/conf.d/net", "")
	listed, err := c.ListFiles(ctx, &v1alpha1.ListFilesRequest{MachineId: "m", Path: "/etc"})
	require.NoError(t, err)
	assert.Equal(t, []*v1alpha1.FileEntry{
		{Path: "/etc/conf.d", Kind: v1alpha1.EntryKind_ENTRY_KIND_DIRECTORY},
		{Path: "/etc/motd", Kind: v1alpha1.EntryKind_ENTRY_KIND_FILE, Size: uint64(len("patched"))},
	}, listed.GetEntries(), "a directory and a file with its size, none of what is below the directory")
	_, err = c.ListFiles(ctx, &v1alpha1.ListFilesRequest{MachineId: "m", Path: "/missing"})
	requireKind(t, err, notFound)
	_, err = c.ListFiles(ctx, &v1alpha1.ListFilesRequest{MachineId: "m", Path: "/etc/motd"})
	requireKind(t, err, rejected)

	t.Run("a Machine that is not Running refuses them", func(t *testing.T) {
		_, err := c.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(0)})
		require.NoError(t, err)
		_, err = c.SearchFiles(ctx, &v1alpha1.SearchFilesRequest{MachineId: "m", Path: "/", Pattern: "x"})
		requireKind(t, err, precondition)
		_, err = c.ListFiles(ctx, &v1alpha1.ListFilesRequest{MachineId: "m", Path: "/"})
		requireKind(t, err, precondition)
		_, err = c.Exec(ctx, execRequest())
		requireKind(t, err, precondition)
	})
}

// Signal through the wired graph: a running command takes it, a command that
// has ended is a failed precondition, and a number that is not a signal is
// rejected.
func TestGuestAPI_SignalExec(t *testing.T) {
	ctx := context.Background()
	n := running(t)
	g := n.app.Substitutes.Guest
	c := n.client
	_, err := c.Exec(ctx, execRequest())
	require.NoError(t, err)

	_, err = c.SignalExec(ctx, &v1alpha1.SignalExecRequest{MachineId: "m", ExecId: "x", Signal: 15})
	require.NoError(t, err)
	assert.Equal(t, []int32{15}, g.Signals("x"))

	_, err = c.SignalExec(ctx, &v1alpha1.SignalExecRequest{MachineId: "m", ExecId: "x"})
	requireKind(t, err, rejected)
	_, err = c.SignalExec(ctx, &v1alpha1.SignalExecRequest{MachineId: "m", ExecId: "other", Signal: 15})
	requireKind(t, err, notFound)
	assert.Equal(t, []int32{15}, g.Signals("x"), "only the valid call reached the guest")

	g.Finish("x", 0)
	_, err = c.SignalExec(ctx, &v1alpha1.SignalExecRequest{MachineId: "m", ExecId: "x", Signal: 15})
	requireKind(t, err, precondition)

	_, err = c.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(0)})
	require.NoError(t, err)
	_, err = c.SignalExec(ctx, &v1alpha1.SignalExecRequest{MachineId: "m", ExecId: "x", Signal: 15})
	requireKind(t, err, precondition)
}

func TestGuestAPI_AnInvalidMessageIsRefused(t *testing.T) {
	ctx := context.Background()
	n := running(t)
	c := n.client

	w, err := c.WriteFile(ctx)
	require.NoError(t, err)
	require.NoError(t, w.Send(&v1alpha1.WriteFileRequest{Part: &v1alpha1.WriteFileRequest_Chunk{Chunk: []byte("no start")}}))
	_, err = w.CloseAndRecv()
	requireKind(t, err, rejected)

	_, err = c.Exec(ctx, &v1alpha1.ExecRequest{ExecId: "x", RequestId: "r", Command: []string{"true"}})
	requireKind(t, err, rejected) // no machine_id
	r, err := c.ReadExecOutput(ctx, &v1alpha1.ReadExecOutputRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = r.Recv()
	requireKind(t, err, rejected) // no exec_id

	// Control: a valid read is not refused.
	ok, err := c.ReadFile(ctx, &v1alpha1.ReadFileRequest{MachineId: "m", Path: "/missing"})
	require.NoError(t, err)
	_, err = ok.Recv()
	requireKind(t, err, notFound)
}
