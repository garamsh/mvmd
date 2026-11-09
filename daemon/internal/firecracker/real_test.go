//go:build firecracker_real

package firecracker_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/channel"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests boot the pinned Firecracker on the pinned kernel with the Guest
// Agent's initrd, as the user running them: no supervisor, no confinement and
// no tap, so they need /dev/kvm and no root. They run through
// `make test-firecracker`, which fetches the inputs and names them; a run
// without them fails, since a pass that never booted a guest would say
// nothing about the Driver.

func input(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: run these tests through `make test-firecracker`, which fetches the inputs and sets it", name)
	}
	return v
}

func TestReal_ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t *testing.T) {
	// The Unix socket paths bound the length of this directory.
	dir, err := os.MkdirTemp("", "fcreal")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run := filepath.Join(dir, "run")
	require.NoError(t, os.Mkdir(run, 0o700))

	d, err := firecracker.New(firecracker.Config{
		Binary: input(t, "MVMD_FIRECRACKER"), BinarySHA256: input(t, "MVMD_FIRECRACKER_SHA256"),
		Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
		Initrd: input(t, "MVMD_INITRD"),
		Dirs:   runDirs{run},
	})
	require.NoError(t, err)

	devices := make([]machine.BlockDevice, 0, 2)
	for i, name := range []string{"image.img", "root.img"} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Truncate(path, 16<<20))
		devices = append(devices, machine.BlockDevice{Disk: machine.DiskID(name), Path: path, ReadOnly: i == 0})
	}
	spec := machine.BootSpec{Devices: devices, Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, RunDir: run}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)

	// What the supervisor does, without its confinement: run the described
	// process with its output in the run directory's log.
	vmmLog, err := os.Create(filepath.Join(run, "vmm.log"))
	require.NoError(t, err)
	defer vmmLog.Close()
	cmd := exec.Command(p.Executable, p.Args...)
	cmd.Stdout, cmd.Stderr = vmmLog, vmmLog
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if t.Failed() {
			if raw, err := os.ReadFile(filepath.Join(run, "vmm.log")); err == nil {
				t.Logf("the VMM's log:\n%s", raw)
			}
		}
	})

	require.NoError(t, d.Boot(ctx, "m1", spec))

	// The Guest Agent starts listening some time after the kernel boots, so a
	// call retries until it answers. Its answer is what proves the whole path:
	// the vsock device, Firecracker's CONNECT handshake, and the agent.
	guest := channel.NewGuest(d)
	require.Eventually(t, func() bool {
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		defer ccancel()
		return guest.Health(cctx, "m1") == nil
	}, time.Minute, 500*time.Millisecond, "the Guest Agent never answered Health through GuestConn")

	// A paused guest runs nothing, so the agent answers nothing until resumed.
	require.NoError(t, d.Pause(ctx, "m1"))
	pctx, pcancel := context.WithTimeout(ctx, 2*time.Second)
	assert.Error(t, guest.Health(pctx, "m1"), "a paused guest does not answer")
	pcancel()
	require.NoError(t, d.Resume(ctx, "m1"))
	require.Eventually(t, func() bool {
		cctx, ccancel := context.WithTimeout(ctx, 5*time.Second)
		defer ccancel()
		return guest.Health(cctx, "m1") == nil
	}, 30*time.Second, 200*time.Millisecond, "the guest did not answer after Resume")

	// Stop: Shutdown asks, and what the guest's kernel does with Ctrl-Alt-Del
	// is the kernel's. The VMM is ended either way, as the supervisor ends it.
	if err := d.Shutdown(ctx, "m1", 2*time.Second); err != nil {
		t.Logf("the guest did not stop gracefully: %v", err)
	}
	require.NoError(t, cmd.Process.Kill())
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("the VMM did not end after being killed")
	}
	_, err = d.GuestConn(ctx, "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable, "no guest is reachable once the VMM has ended")
}
