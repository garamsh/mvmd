package machine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// badDiskIDs are identifiers that cannot name a disk file, each with a name
// for the subtest.
var badDiskIDs = []struct {
	name string
	id   machine.DiskID
}{
	{"empty", ""},
	{"a dot", "."},
	{"two dots", ".."},
	{"a slash", "a/b"},
	{"a NUL", "a\x00b"},
	{"the temporary prefix", "tmp-x"},
	{"256 bytes", machine.DiskID(strings.Repeat("a", 256))},
}

// goodDiskIDs are identifiers everything around the rule accepts, the longest
// that fits among them.
var goodDiskIDs = []machine.DiskID{"data", "a.b_C-1", "..a", "tmpx", machine.DiskID(strings.Repeat("a", 255))}

func TestValidDiskID(t *testing.T) {
	for _, tt := range badDiskIDs {
		assert.False(t, machine.ValidDiskID(tt.id), tt.name)
	}
	for _, id := range goodDiskIDs {
		assert.True(t, machine.ValidDiskID(id), "%.20q", id)
	}
}

// requireUntouched asserts no call reached the Disk port at the steps.
func requireUntouched(t *testing.T, e *env, steps ...string) {
	t.Helper()
	for _, step := range steps {
		assert.Zero(t, e.disks.Calls(step), "the Disk port saw %q", step)
	}
}

// The identifier's form is the domain's: every operation that takes a client's
// disk identifier refuses a bad one as invalid before it calls a port.
func TestCreateDisk_DiskIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			requireKind(t, e.svc.CreateDisk(ctx, tt.id, "req", 1<<20), machine.ErrInvalid)
			requireUntouched(t, e, "get", "create")
			_, held := e.disks.Holds(tt.id)
			assert.False(t, held)

			for _, ok := range goodDiskIDs {
				require.NoError(t, e.svc.CreateDisk(ctx, ok, "req", 1<<20), "control %.20q", ok)
			}
		})
	}
}

func TestDeleteDisk_DiskIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			requireKind(t, e.svc.DeleteDisk(ctx, tt.id), machine.ErrInvalid)
			requireUntouched(t, e, "remove")

			for _, ok := range goodDiskIDs {
				require.NoError(t, e.svc.CreateDisk(ctx, ok, "req", 1<<20))
				require.NoError(t, e.svc.DeleteDisk(ctx, ok), "control %.20q", ok)
				_, held := e.disks.Holds(ok)
				assert.False(t, held)
			}
		})
	}
}

func TestAttachDisk_DiskIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.created(t, "m")
			requireKind(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: tt.id}), machine.ErrInvalid)
			requireUntouched(t, e, "locate", "give")
			assert.Empty(t, e.requireState(t, "m", machine.StateCreated).Disks)

			for _, ok := range goodDiskIDs {
				e.disks.Provide(ok)
				require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: ok}), "control %.20q", ok)
			}
			assert.Len(t, e.requireState(t, "m", machine.StateCreated).Disks, len(goodDiskIDs))
		})
	}
}

func TestDetachDisk_DiskIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.created(t, "m")
			requireKind(t, e.svc.DetachDisk(ctx, "m", tt.id), machine.ErrInvalid)
			requireUntouched(t, e, "reclaim")

			for _, ok := range goodDiskIDs {
				e.disks.Provide(ok)
				require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: ok}))
				require.NoError(t, e.svc.DetachDisk(ctx, "m", ok), "control %.20q", ok)
			}
			assert.Empty(t, e.requireState(t, "m", machine.StateCreated).Disks)
			assert.Equal(t, len(goodDiskIDs), e.disks.Calls("reclaim"), "a good identifier reaches the port")
		})
	}
}

func TestCreate_DiskIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			spec := baseSpec()
			spec.Disks = []machine.DiskAttachment{{Disk: tt.id}}
			requireKind(t, e.svc.Create(ctx, "m", "req", spec), machine.ErrInvalid)
			e.requireNoMachine(t, "m")
			_, recorded := e.store.Record("m")
			assert.False(t, recorded, "nothing is recorded")
			requireUntouched(t, e, "locate", "create-root", "give")

			for i, ok := range goodDiskIDs {
				e.disks.Provide(ok)
				id := machine.MachineID(fmt.Sprintf("c%d", i))
				spec.Disks = []machine.DiskAttachment{{Disk: ok}}
				require.NoError(t, e.svc.Create(ctx, id, "req", spec), "control %.20q", ok)
			}
		})
	}
}

// The real Disk adapter answers a bad identifier from Get with ErrDiskInvalid,
// which the in-memory one never does, so the service is built on it here.
func TestCreateDisk_RealAdapterRefusesAnIdentifierThatNamesNoFile(t *testing.T) {
	ctx := context.Background()
	disk, err := diskfile.NewDisk(t.TempDir(), "/bin/true")
	require.NoError(t, err)
	e := newEnv(machine.CapabilityDiskHotplug)
	svc := machine.NewService(e.driver, e.sup, e.net, disk, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()))

	for _, tt := range badDiskIDs {
		t.Run(tt.name, func(t *testing.T) {
			requireKind(t, svc.CreateDisk(ctx, tt.id, "req", 1<<20), machine.ErrInvalid)
			requireKind(t, svc.DeleteDisk(ctx, tt.id), machine.ErrInvalid)

			spec := baseSpec()
			spec.Disks = []machine.DiskAttachment{{Disk: tt.id}}
			requireKind(t, svc.Create(ctx, "m", "req", spec), machine.ErrInvalid)
			_, recorded := e.store.Record("m")
			assert.False(t, recorded, "no failed Machine is recorded")
		})
	}
	entries, err := disk.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, entries, "nothing was written")
}
