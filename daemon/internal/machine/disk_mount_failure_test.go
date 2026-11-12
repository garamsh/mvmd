package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A disk the adapter cannot read, when a mount path asks what it holds, fails
// Create as locating it does: the failure is recorded under the request ID, and
// a repeat answers it again though the adapter has healed.
func TestCreate_AFailureToReadAMountedDiskIsRecordedUnderTheRequest(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{mounted("d", "/data")}
	e.disks.FailAt("get", errBoom)

	first := e.svc.Create(ctx, "m", "r", spec)
	requireKind(t, first, machine.ErrFailed)
	e.requireNoMachine(t, "m")

	e.disks.Heal("get")
	again := e.svc.Create(ctx, "m", "r", spec)
	requireKind(t, again, machine.ErrFailed)
	assert.Equal(t, first, again, "the original outcome, reason included")

	// Control: another request ID is a new call, and with the adapter healed it creates.
	require.NoError(t, e.svc.Create(ctx, "m", "r2", spec))
}

// A refusal of the request itself is not recorded: the same request ID,
// corrected, is a new call.
func TestCreate_ARefusalOfAMountPathIsNotRecordedUnderTheRequest(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	require.NoError(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemNone))
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{mounted("raw", "/data")}
	requireKind(t, e.svc.Create(ctx, "m", "r", spec), machine.ErrInvalid)

	spec.Disks = []machine.DiskAttachment{rw("raw")}
	require.NoError(t, e.svc.Create(ctx, "m", "r", spec), "the same request ID, corrected, is a new call")
}
