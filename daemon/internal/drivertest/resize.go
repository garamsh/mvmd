package drivertest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

// ceiling is the most the Machine of the resize scenario can have: more vCPUs
// and more memory than it boots with (ADR 0029).
var ceiling = machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}

// AMachineIsResizedWhileRunningAndItsGuestSeesTheNewSize boots a Machine with a
// ceiling above its resources, resizes it to the ceiling, and has the Guest
// Agent bring the new vCPUs online (ADR 0029): the guest then sees the new vCPU
// count and more memory than it booted with. A resize repeated converges, and a shrink back to the resources returns the guest to its boot figures. A
// Driver that reports no resize capability is held to refusing the resize
// instead.
func AMachineIsResizedWhileRunningAndItsGuestSeesTheNewSize(t *testing.T, s Subject) {
	dir := tempDir(t, "dtresize")
	run := statedir.Run(dir)
	require.NoError(t, os.Mkdir(run, 0o700))
	d := s.Driver(t, RunDirs{"m1": run})

	image, root := diskFile(t, dir, "image", true), diskFile(t, dir, "root", false)
	image.Disk = ""
	spec := machine.BootSpec{Devices: []machine.BlockDevice{image, root}, Resources: resources, Ceiling: ceiling, RunDir: run}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startLogged(t, s, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := s.Guest(d)
	require.Eventually(t, func() bool { return healthy(ctx, guest, "m1", 5*time.Second) }, time.Minute, 500*time.Millisecond, "the Guest Agent never answered")
	online := func(vcpus uint32) (machine.GuestSize, error) {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return guest.OnlineVcpus(cctx, "m1", vcpus)
	}

	// Control: before any resize the guest has the vCPUs it booted with, and
	// the agent onlines no more than are present, so what it reports after the
	// resize is the resize's.
	before, err := online(ceiling.VCPUs)
	require.NoError(t, err)
	require.Equal(t, resources.VCPUs, before.VCPUs, "the guest booted with %d vCPUs", resources.VCPUs)

	if !has(d, machine.CapabilityResize) {
		assert.ErrorIs(t, d.Resize(ctx, "m1", ceiling), machine.ErrUnsupported, "Resize")
		after, err := online(ceiling.VCPUs)
		require.NoError(t, err)
		assert.Equal(t, before, after, "a refused resize changed nothing")
		return
	}

	require.Positive(t, before.MemoryBytes, "the guest reports the memory it holds")

	// Memory that is not a multiple of 2 MiB is refused and changes nothing.
	require.Error(t, d.Resize(ctx, "m1", machine.Resources{VCPUs: ceiling.VCPUs, MemoryBytes: ceiling.MemoryBytes - 1<<20}))
	unchanged, err := online(ceiling.VCPUs)
	require.NoError(t, err)
	require.Equal(t, before, unchanged, "a refused resize changed nothing")

	require.NoError(t, d.Resize(ctx, "m1", ceiling))
	require.NoError(t, d.Resize(ctx, "m1", ceiling), "a repeat converges")

	// The guest reports both figures the resize changed, so the wait is on both.
	added := ceiling.MemoryBytes - resources.MemoryBytes
	var after machine.GuestSize
	require.Eventually(t, func() bool {
		after, err = online(ceiling.VCPUs)
		return err == nil && after.VCPUs == ceiling.VCPUs && after.MemoryBytes > before.MemoryBytes+added/2
	}, time.Minute, 500*time.Millisecond, "the guest did not reach %d vCPUs and the added memory: %v, %+v", ceiling.VCPUs, err, after)
	assert.True(t, healthy(ctx, guest, "m1", 5*time.Second), "the guest still answers after the resize")

	// A shrink back to the resources: the VMM removes the vCPUs and unplugs the
	// memory, and the guest reports the boot count and less memory than grown.
	require.NoError(t, d.Resize(ctx, "m1", resources))
	var shrunk machine.GuestSize
	require.Eventually(t, func() bool {
		shrunk, err = online(ceiling.VCPUs)
		return err == nil && shrunk.VCPUs == resources.VCPUs && shrunk.MemoryBytes < after.MemoryBytes-added/2
	}, time.Minute, 500*time.Millisecond, "the guest did not give up the vCPUs and memory: %v, %+v after %+v", err, shrunk, after)
	assert.True(t, healthy(ctx, guest, "m1", 5*time.Second), "the guest still answers after the shrink")
}
