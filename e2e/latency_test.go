//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// The steps of the cycle the test times, in the order they run.
var latencySteps = []string{"start", "pause", "snapshot", "stop", "delete", "restore", "exec"}

// A Machine of 1 vCPU, 512 MiB and a 1 GiB root disk is started cold, hibernated (paused,
// snapshotted, stopped, deleted) and restored, five times over. Each call's wall-clock duration at
// the client is logged with the Driver, the memory size and the snapshot's size, and the minimum,
// median and maximum of each step after the last cycle. No time bound is asserted: each step
// succeeds, and a file written before the snapshot is read after the restore.
func TestE2E_AMachineIsStartedHibernatedAndRestoredFiveTimesAndEachStepIsTimed(t *testing.T) {
	const (
		cycles = 5
		marker = "written before the snapshot"
	)
	memory := uint64(512 << 20)
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "latency")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	took := map[string][]time.Duration{}
	for i := 1; i <= cycles; i++ {
		origin, woken, snapshot := fmt.Sprintf("lat-%d", i), fmt.Sprintf("lat-%d-woken", i), fmt.Sprintf("lat-%d-snap", i)
		var snapshotBytes uint64
		durations := map[string]time.Duration{}
		timed := func(step string, call func()) {
			begin := time.Now()
			call()
			durations[step] = time.Since(begin)
			took[step] = append(took[step], durations[step])
		}

		req := createRequest(origin, pulled.Digest)
		req.Resources = &v1alpha2.Resources{CpuCount: 1, MemoryBytes: memory}
		req.RootDiskBytes = 1 << 30
		created, err := n.client.CreateMachine(ctx, req)
		require.NoError(t, err)
		require.Equal(t, uint32(1), created.Machine.Resources.CpuCount, "the Machine has the vCPU count the test asked for")
		require.Equal(t, memory, created.Machine.Resources.MemoryBytes, "the Machine has the memory the test asked for")

		timed("start", func() {
			started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: origin})
			require.NoError(t, err)
			require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
		})
		n.writeFile(t, ctx, origin, "/marker", marker)

		timed("pause", func() {
			_, err := n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: origin})
			require.NoError(t, err)
		})
		timed("snapshot", func() {
			_, err := n.client.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{SnapshotId: snapshot, RequestId: "snapshot-" + snapshot, MachineId: origin})
			require.NoError(t, err)
		})
		for _, listed := range n.listedSnapshots(t, ctx) {
			if listed.Id == snapshot {
				snapshotBytes = listed.SizeBytes
			}
		}
		require.NotZero(t, snapshotBytes, "List snapshots lists the snapshot with its size")
		timed("stop", func() {
			_, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: origin, GracePeriod: durationpb.New(10 * time.Second)})
			require.NoError(t, err)
		})
		timed("delete", func() {
			_, err := n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: origin})
			require.NoError(t, err)
		})

		timed("restore", func() {
			restoreCtx, cancelRestore := context.WithTimeout(ctx, 3*time.Minute)
			defer cancelRestore()
			restored, err := n.client.RestoreMachine(restoreCtx, &v1alpha2.RestoreMachineRequest{MachineId: woken, RequestId: "restore-" + woken, SnapshotId: snapshot})
			require.NoError(t, err)
			require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, restored.Machine.State)
		})
		timed("exec", func() {
			out, code := n.exec(t, ctx, woken, "echo-"+woken, "/bin/entry", "echo", "restored")
			require.Equal(t, int32(0), code, "echo: %s", out)
			require.Equal(t, "restored\n", out, "the restored guest answers an exec")
		})
		require.Equal(t, marker, n.readFile(t, ctx, woken, "/marker"), "the restored guest holds the file the origin wrote before the snapshot")

		for _, step := range latencySteps {
			t.Logf("latency driver=%s cycle=%d step=%s duration_ms=%.1f memory_bytes=%d snapshot_bytes=%d", driver.driver, i, step, millis(durations[step]), memory, snapshotBytes)
		}

		_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: woken, GracePeriod: durationpb.New(10 * time.Second)})
		require.NoError(t, err)
		_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: woken})
		require.NoError(t, err)
		_, err = n.client.DeleteSnapshot(ctx, &v1alpha2.DeleteSnapshotRequest{SnapshotId: snapshot})
		require.NoError(t, err)
	}

	for _, step := range latencySteps {
		sorted := slices.Clone(took[step])
		slices.Sort(sorted)
		require.Len(t, sorted, cycles, "step %s ran in every cycle", step)
		t.Logf("latency-summary driver=%s step=%s cycles=%d min_ms=%.1f median_ms=%.1f max_ms=%.1f memory_bytes=%d", driver.driver, step, cycles, millis(sorted[0]), millis(sorted[cycles/2]), millis(sorted[cycles-1]), memory)
	}
	assert.Empty(t, n.listedSnapshots(t, ctx), "every cycle's snapshot was deleted")
}

func millis(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}
