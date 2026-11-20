//go:build e2e_real

package e2e_test

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// Reproduction of #743: stop with grace, start, console, N times; counts the sessions that end in an error.
func TestE2E_Repro743(t *testing.T) {
	const diskBytes = 32 << 20
	const rounds = 40
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet))
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Minute)
	defer cancel()
	_, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-ro", RequestId: "create-os-ro", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-rw", RequestId: "create-os-rw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId: "os", RequestId: "create-os",
		Resources: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		OsBoot:    &v1alpha2.OsBoot{BootSet: osFormSet, KernelCmdline: "mvmd.token=" + token},
		Disks:     []*v1alpha2.DiskAttachment{{DiskId: "os-ro", ReadOnly: true}, {DiskId: "os-rw"}},
	})
	require.NoError(t, err)

	failures := 0
	for i := 0; i < rounds; i++ {
		if i > 0 {
			_, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "os", GracePeriod: durationpb.New(3 * time.Second)})
			require.NoError(t, err)
			n.waitVMMGone(t, "os")
		}
		began := time.Now()
		_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "os"})
		require.NoError(t, err)
		startedAfter := time.Since(began)
		pidsAtStart := n.vmmPIDs(t, "os")
		err = n.repro743Read(ctx)
		if err == nil {
			t.Logf("REPRO743 round %d ok", i)
			continue
		}
		failures++
		inspected, ierr := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "os"})
		state := "?"
		if ierr == nil {
			state = inspected.Machine.State.String()
		}
		t.Logf("REPRO743 round %d FAILED: %v; start took %v, read ended %v after start; state now %s; VMM pids at start %v, now %v",
			i, err, startedAfter, time.Since(began), state, pidsAtStart, n.vmmPIDs(t, "os"))
		time.Sleep(time.Second)
		for _, name := range []string{"supervisor.log", filepath.Join("run", "vmm.log")} {
			raw, rerr := os.ReadFile(filepath.Join(n.stateDir, "machines", "os", name))
			if len(raw) > 4000 {
				raw = raw[len(raw)-4000:]
			}
			t.Logf("REPRO743 round %d %s (tail, err %v):\n%s", i, name, rerr, raw)
		}
	}
	t.Logf("REPRO743 RESULT: %d of %d starts ended their console in an error", failures, rounds)
	if failures > 0 {
		t.Errorf("REPRO743: %d of %d", failures, rounds)
	}
}

func (n *node) repro743Read(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	stream, err := n.client.Console(ctx)
	if err != nil {
		return err
	}
	if err := stream.Send(&v1alpha2.ConsoleRequest{MachineId: "os"}); err != nil {
		return err
	}
	var seen bytes.Buffer
	for {
		part, err := stream.Recv()
		if err != nil {
			return fmt.Errorf("%w; %d bytes seen, tail %q", err, seen.Len(), tailOf(seen.Bytes(), 200))
		}
		seen.Write(part.GetOutput())
		if osFormLine.Match(seen.Bytes()) {
			return nil
		}
	}
}

func tailOf(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}
