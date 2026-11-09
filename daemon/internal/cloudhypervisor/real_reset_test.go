//go:build cloud_hypervisor_real

package cloudhypervisor_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/drivertest"
	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// newc writes an initrd of one file and no init, in the cpio format the kernel
// unpacks (the "newc" archive of its Documentation/driver-api/early-userspace).
func newc(name string, data []byte) []byte {
	pad := func(b []byte) []byte {
		for len(b)%4 != 0 {
			b = append(b, 0)
		}
		return b
	}
	entry := func(name string, mode, size int, data []byte) []byte {
		hdr := fmt.Sprintf("070701%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x%08x",
			0, mode, 0, 0, 1, 0, size, 0, 0, 0, 0, len(name)+1, 0)
		return append(pad(append([]byte(hdr+name), 0)), pad(data)...)
	}
	out := entry(name, 0o100644, len(data), data)
	return append(out, entry("TRAILER!!!", 0, 0, nil)...)
}

// A guest that panics ends its VMM. The guest's initrd holds no init, so the
// kernel cannot mount a root and panics, which the Driver's command line
// (panic=1) turns into a reset after a second; Cloud Hypervisor survives a
// reset by rebooting the guest, and the supervisor's event pipe, which this
// test runs as the supervisor does, ends the VMM on the reset (ADR 0026). No
// change of the Guest Agent is needed to panic a guest. A control guest, with
// the Guest Agent's initrd, is started beside it with the same Driver
// configuration and is still up and answering when the other has ended.
func TestReal_AGuestThatPanicsEndsItsVMMAndAGuestThatStaysUpDoesNot(t *testing.T) {
	const bound = 30 * time.Second

	state, err := os.MkdirTemp("", "chreset")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(state) })
	dirs := drivertest.RunDirs{}
	for _, id := range []machine.MachineID{"control", "panics"} {
		dirs[id] = filepath.Join(state, string(id))
		require.NoError(t, os.Mkdir(dirs[id], 0o700))
	}
	noInit := filepath.Join(state, "no-init.cpio")
	require.NoError(t, os.WriteFile(noInit, newc("not-init", []byte("x")), 0o600))

	driver := func(initrd string) machine.Driver {
		d, err := cloudhypervisor.New(cloudhypervisor.Config{
			Binary: input(t, "MVMD_CLOUD_HYPERVISOR"), BinarySHA256: input(t, "MVMD_CLOUD_HYPERVISOR_SHA256"),
			Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
			Initrd: initrd,
			Dirs:   dirs,
		})
		require.NoError(t, err)
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// start describes and starts the Machine, with its VMM's output in its log.
	start := func(id machine.MachineID, d machine.Driver) (drivertest.Process, machine.BootSpec, string) {
		run := dirs[id]
		disk := filepath.Join(state, string(id)+".img")
		require.NoError(t, os.WriteFile(disk, nil, 0o600))
		require.NoError(t, os.Truncate(disk, 16<<20))
		spec := machine.BootSpec{
			Devices:   []machine.BlockDevice{{Disk: "root", Path: disk}},
			Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20},
			RunDir:    run,
		}
		p, err := d.Describe(ctx, id, spec)
		require.NoError(t, err)
		logPath := filepath.Join(run, "vmm.log")
		log, err := os.Create(logPath)
		require.NoError(t, err)
		t.Cleanup(func() { _ = log.Close() })
		proc := drivertest.StartInRoot(t, p.For(spec), drivertest.Streams{Out: log, Err: log})
		t.Cleanup(func() {
			_ = proc.Kill()
			<-proc.Exited()
			if t.Failed() {
				if raw, err := os.ReadFile(logPath); err == nil {
					t.Logf("the log of %s (%s):\n%s", id, logPath, raw)
				}
			}
		})
		return proc, spec, logPath
	}

	good := driver(input(t, "MVMD_INITRD"))
	control, controlSpec, _ := start("control", good)
	require.NoError(t, good.Boot(ctx, "control", controlSpec))
	panicProc, _, panicLog := start("panics", driver(noInit))
	began := time.Now()

	guest := guestchannel.NewGuest(good)
	answers := func() bool {
		cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return guest.Health(cctx, "control") == nil
	}
	require.Eventually(t, answers, time.Minute, 500*time.Millisecond, "the control guest never answered")

	select {
	case <-panicProc.Exited():
		t.Logf("the VMM of the panicking guest ended %v after it started", time.Since(began))
	case <-time.After(bound):
		t.Fatalf("the VMM of the panicking guest was still running %v after it started", bound)
	}
	raw, err := os.ReadFile(panicLog)
	require.NoError(t, err)
	assert.Contains(t, string(raw), "Kernel panic", "what ended the VMM was the guest's panic")
	assert.Contains(t, string(raw), "ending the VMM", "and the supervisor recorded why it ended it")

	select {
	case <-control.Exited():
		t.Fatal("the control guest's VMM ended with the other's")
	default:
	}
	assert.True(t, answers(), "control: the guest that did not panic still answers")
}
