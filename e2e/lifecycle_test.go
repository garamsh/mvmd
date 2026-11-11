//go:build e2e_real

package e2e_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A Machine of a pulled image runs a command, pauses, is snapshotted and restored as a new Machine
// that holds the guest's state, and stops, with its root disk and the snapshot on the node's real
// Disk and Snapshot Store under the state directory. The snapshot is listed with what describes
// it, and Delete removes it from the list and the store.
func TestE2E_AMachineIsSnapshottedAndRestoredAsANewMachineOnTheRealStores(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "lifecycle")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// The control of the two directory checks below: before any Machine and snapshot there is
	// nothing in them, so what shows up afterwards was put there by the call.
	require.Empty(t, n.entries(t, "disks"), "a node with no Machine holds no root disk")
	require.Empty(t, n.listedSnapshots(t, ctx), "a node with no snapshot holds none")

	created, err := n.client.CreateMachine(ctx, createRequest("origin", pulled.Digest))
	require.NoError(t, err)
	rootDisks := n.entries(t, "disks")
	require.Len(t, rootDisks, 1, "the created Machine's root disk is a file under <state_dir>/disks")
	info, err := os.Stat(filepath.Join(n.stateDir, "disks", rootDisks[0]))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular())
	assert.Equal(t, int64(1<<30), info.Size(), "the root disk is the size the Machine was created with")

	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "origin"})
	require.NoError(t, err)

	// The exit code is read from the command: the first exits 0, the control exits 3.
	out, code := n.exec(t, ctx, "origin", "echo-1", "/bin/entry", "echo", "hello", "exec")
	assert.Equal(t, "hello exec\n", out)
	assert.Equal(t, int32(0), code)
	_, code = n.exec(t, ctx, "origin", "exit-3", "/bin/entry", "exit", "3")
	assert.Equal(t, int32(3), code)

	n.writeFile(t, ctx, "origin", "/marker", "written before the snapshot")

	paused, err := n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: "origin"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_PAUSED, paused.Machine.State)

	_, err = n.client.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "snapshot-snap", MachineId: "origin"})
	require.NoError(t, err)
	snapshotDisks, err := filepath.Glob(filepath.Join(n.stateDir, "snapshots", "*", "root.disk"))
	require.NoError(t, err)
	assert.Len(t, snapshotDisks, 1, "the snapshot is a directory under <state_dir>/snapshots holding a copy of the root disk")
	snapshots := n.listedSnapshots(t, ctx)
	require.Len(t, snapshots, 1, "List snapshots lists the snapshot the call took")
	listed := snapshots[0]
	assert.Equal(t, "snap", listed.Id)
	assert.Equal(t, "origin", listed.MachineId)
	assert.Equal(t, pulled.Digest, listed.ImageDigest)
	assert.Equal(t, uint64(1<<30), listed.RootDiskBytes)
	runtimeInfo, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, runtimeInfo.Drivers[0].Driver.Name, listed.Driver.Name, "the Driver that made the snapshot")
	assert.Equal(t, runtimeInfo.Drivers[0].Driver.Version, listed.Driver.Version)
	assert.NotEmpty(t, listed.Driver.Version)
	assert.Equal(t, runtimeInfo.Drivers[0].CpuModel, listed.CpuModel, "the CPU model a restore compares")
	assert.NotEmpty(t, listed.CpuModel)
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "origin"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_PAUSED, inspected.Machine.State, "a snapshot leaves the Machine in the state it was in")

	// A restore that never returns fails the test, with the node's logs, well before the test
	// binary's own timeout would kill it with none.
	restoreCtx, cancelRestore := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelRestore()
	restored, err := n.client.RestoreMachine(restoreCtx, &v1alpha2.RestoreMachineRequest{MachineId: "copy", RequestId: "restore-copy", SnapshotId: "snap"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, restored.Machine.State)
	assert.NotEqual(t, created.Machine.Addresses, restored.Machine.Addresses, "the restored Machine has an address of its own")
	assert.Len(t, n.entries(t, "disks"), 2, "the restored Machine has a root disk of its own beside the origin's")

	assert.Equal(t, "written before the snapshot", n.readFile(t, ctx, "copy", "/marker"), "the restored guest holds what the origin's did")
	out, code = n.exec(t, ctx, "copy", "echo-2", "/bin/entry", "echo", "restored")
	assert.Equal(t, "restored\n", out)
	assert.Equal(t, int32(0), code)

	for _, id := range []string{"copy", "origin"} {
		stopped, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: id, GracePeriod: durationpb.New(10 * time.Second)})
		require.NoError(t, err, "stop %s", id)
		assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stopped.Machine.State, id)
	}
	assert.Len(t, n.entries(t, "disks"), 2, "a stop retains the disks")
	assert.Len(t, n.listedSnapshots(t, ctx), 1, "a stop leaves the snapshot")

	// The snapshot is in the store until Delete removes it, with its files, and a repeat that
	// finds it absent succeeds.
	require.Len(t, n.snapshots(t), 1)
	_, err = n.client.DeleteSnapshot(ctx, &v1alpha2.DeleteSnapshotRequest{SnapshotId: "snap"})
	require.NoError(t, err)
	assert.Empty(t, n.listedSnapshots(t, ctx), "a deleted snapshot is not listed")
	assert.Empty(t, n.snapshots(t), "the deleted snapshot's directory is gone from the store")
	_, err = n.client.DeleteSnapshot(ctx, &v1alpha2.DeleteSnapshotRequest{SnapshotId: "snap"})
	assert.NoError(t, err, "a snapshot that is absent is deleted with no change")
}

// listedSnapshots is what List snapshots answers.
func (n *node) listedSnapshots(t *testing.T, ctx context.Context) []*v1alpha2.Snapshot {
	t.Helper()
	listed, err := n.client.ListSnapshots(ctx, &v1alpha2.ListSnapshotsRequest{})
	require.NoError(t, err)
	return listed.Snapshots
}

// entries lists the names in a directory of the state directory.
func (n *node) entries(t *testing.T, name string) []string {
	t.Helper()
	items, err := os.ReadDir(filepath.Join(n.stateDir, name))
	require.NoError(t, err)
	var names []string
	for _, item := range items {
		names = append(names, item.Name())
	}
	return names
}

// snapshots lists the snapshots of the Snapshot Store in the state directory, which are its
// directories but for the one it builds them in.
func (n *node) snapshots(t *testing.T) []string {
	t.Helper()
	var names []string
	for _, name := range n.entries(t, "snapshots") {
		if name != "staging" {
			names = append(names, name)
		}
	}
	return names
}

// exec runs the command in the Machine, reads its output until it has ended, and returns the
// output and the exit code.
func (n *node) exec(t *testing.T, ctx context.Context, machine, id string, command ...string) (string, int32) {
	t.Helper()
	started, err := n.client.Exec(ctx, &v1alpha2.ExecRequest{ExecId: id, RequestId: "exec-" + id, MachineId: machine, Command: command})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_STARTED, started.Record.Status, started.Record.Reason)

	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	var out strings.Builder
	record, err := v1alpha2.ReadExecOutput(ctx, n.client, machine, id, 0, func(c *v1alpha2.OutputChunk) error {
		out.Write(c.GetData())
		return nil
	})
	require.NoError(t, err, "the output of %s so far: %q", id, out.String())
	require.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, record.Status, record.Reason)
	return out.String(), record.ExitCode
}

func (n *node) writeFile(t *testing.T, ctx context.Context, machine, path, content string) {
	t.Helper()
	stream, err := n.client.WriteFile(ctx)
	require.NoError(t, err)
	require.NoError(t, stream.Send(&v1alpha2.WriteFileRequest{Part: &v1alpha2.WriteFileRequest_Start{Start: &v1alpha2.WriteFileStart{MachineId: machine, Path: path}}}))
	require.NoError(t, stream.Send(&v1alpha2.WriteFileRequest{Part: &v1alpha2.WriteFileRequest_Chunk{Chunk: []byte(content)}}))
	_, err = stream.CloseAndRecv()
	require.NoError(t, err)
}

func (n *node) readFile(t *testing.T, ctx context.Context, machine, path string) string {
	t.Helper()
	stream, err := n.client.ReadFile(ctx, &v1alpha2.ReadFileRequest{MachineId: machine, Path: path})
	require.NoError(t, err)
	var out strings.Builder
	for {
		part, err := stream.Recv()
		if err == io.EOF {
			return out.String()
		}
		require.NoError(t, err)
		out.Write(part.Chunk)
	}
}
