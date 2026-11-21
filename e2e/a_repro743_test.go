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

// Reproduction of #743, round 2. Throwaway: never merged.

const repro743Rounds = 30

func tailOf(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

func (n *node) repro743Disks(t *testing.T, ctx context.Context, token string, restart bool) {
	const diskBytes = 32 << 20
	_, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-ro", RequestId: "create-os-ro", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "os-rw", RequestId: "create-os-rw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId: "os", RequestId: "create-os",
		Resources:     &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		OsBoot:        &v1alpha2.OsBoot{BootSet: osFormSet, KernelCmdline: "mvmd.token=" + token},
		Disks:         []*v1alpha2.DiskAttachment{{DiskId: "os-ro", ReadOnly: true}, {DiskId: "os-rw"}},
		RestartOnBoot: restart,
	})
	require.NoError(t, err)
}

// repro743Dump logs the Machine's state and the tails of the logs that name why a session ended.
func (n *node) repro743Dump(t *testing.T, label string) {
	time.Sleep(time.Second)
	for _, name := range []string{"supervisor.log", filepath.Join("run", "vmm.log")} {
		raw, rerr := os.ReadFile(filepath.Join(n.stateDir, "machines", "os", name))
		t.Logf("REPRO743 %s %s (tail, err %v):\n%s", label, name, rerr, tailOf(raw, 5000))
	}
	t.Logf("REPRO743 %s daemon log tail:\n%s", label, tailOf([]byte(n.log()), 3000))
}

// repro743Read reads a new session until the init's line, or returns why it ended first.
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

// repro743Hold opens a session that stays open until the context ends, and reports on done why it ended.
func (n *node) repro743Hold(ctx context.Context) (lineSeen <-chan struct{}, done <-chan error) {
	line := make(chan struct{})
	end := make(chan error, 1)
	go func() {
		stream, err := n.client.Console(ctx)
		if err != nil {
			end <- err
			return
		}
		if err := stream.Send(&v1alpha2.ConsoleRequest{MachineId: "os"}); err != nil {
			end <- err
			return
		}
		var seen bytes.Buffer
		found := false
		for {
			part, err := stream.Recv()
			if err != nil {
				end <- err
				return
			}
			if !found {
				seen.Write(part.GetOutput())
				if osFormLine.Match(seen.Bytes()) {
					found = true
					close(line)
				}
			}
		}
	}()
	return line, end
}

// A session held open across the Stop, and a new session on the next boot.
func TestE2E_Repro743AcrossStop(t *testing.T) {
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet))
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	n.repro743Disks(t, ctx, token, false)

	failures, rounds := 0, 0
	for i := 0; i < repro743Rounds; i++ {
		if ctx.Err() != nil {
			break
		}
		rounds++
		began := time.Now()
		_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "os"})
		require.NoError(t, err)
		holdCtx, holdCancel := context.WithCancel(ctx)
		lineSeen, held := n.repro743Hold(holdCtx)
		select {
		case <-lineSeen:
		case err := <-held:
			failures++
			t.Logf("REPRO743 round %d FAILED (the held session) after %v: %v", i, time.Since(began), err)
			n.repro743Dump(t, fmt.Sprintf("round %d held", i))
		case <-time.After(2 * time.Minute):
			t.Fatalf("round %d: no line", i)
		}
		_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "os", GracePeriod: durationpb.New(3 * time.Second)})
		require.NoError(t, err)
		n.waitVMMGone(t, "os")
		select {
		case err := <-held:
			t.Logf("REPRO743 round %d: the held session ended with %v, %v after the start", i, err, time.Since(began))
		case <-time.After(5 * time.Second):
			t.Logf("REPRO743 round %d: the held session did not end within 5s of the VMM's end", i)
		}
		holdCancel()
	}
	t.Logf("REPRO743 RESULT across-stop: %d of %d rounds failed", failures, rounds)
	if failures > 0 {
		t.Errorf("REPRO743: %d of %d", failures, rounds)
	}
}

// The daemon stopped and started again between boots, the host's boot changing, with no Stop.
func TestE2E_Repro743DaemonRestart(t *testing.T) {
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet), withBootID(firstBootID))
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	n.repro743Disks(t, ctx, token, true)
	_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "os"})
	require.NoError(t, err)
	require.NoError(t, n.repro743Read(ctx))

	failures, rounds := 0, 0
	for i := 0; i < repro743Rounds; i++ {
		if ctx.Err() != nil {
			break
		}
		rounds++
		n.stopDaemon()
		n.killSupervisorsAndVMMs()
		n.waitVMMGone(t, "os")
		n.setBootID(fmt.Sprintf("%08x-2222-4222-8222-222222222222\n", i+1))
		n.startDaemon()
		began := time.Now()
		n.renew(t, time.Hour, 1)
		n.waitState(t, ctx, "os", v1alpha2.MachineState_MACHINE_STATE_RUNNING)
		if err := n.repro743Read(ctx); err != nil {
			failures++
			t.Logf("REPRO743 round %d FAILED after %v: %v", i, time.Since(began), err)
			n.repro743Dump(t, fmt.Sprintf("round %d restart", i))
		}
	}
	t.Logf("REPRO743 RESULT daemon-restart: %d of %d rounds failed", failures, rounds)
	if failures > 0 {
		t.Errorf("REPRO743: %d of %d", failures, rounds)
	}
}
