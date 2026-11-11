package machine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// longest is the longest Machine identifier whose root disk name fits one path
// component.
var longest = machine.MachineID(strings.Repeat("a", 250))

// unusableIDs are identifiers the node cannot derive its names from, each with
// the identifier everything around it accepts.
var unusableIDs = []struct {
	name string
	id   machine.MachineID
}{
	{"a dot", "."},
	{"two dots", ".."},
	{"a slash", "a/b"},
	{"a NUL", "a\x00b"},
	{"a root disk name that starts with tmp-", "tmp-a"},
	{"tmp, whose root disk name is tmp-root", "tmp"},
	{"one byte too long", longest + "a"},
}

// accepted are identifiers the node takes, the longest that fits among them.
var accepted = []machine.MachineID{"a.b_C-1", longest, "..a", "tmpx"}

// Row "Create Machine", column Needs: a form for the identifier.
func TestCreate_MachineIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range unusableIDs {
		t.Run(tt.name, func(t *testing.T) {
			// One UID: a refusal that took it would fail the control below.
			e := newEnvWithRange(machine.UIDRange{First: 2000, Count: 1})
			err := e.svc.Create(ctx, tt.id, "r", baseSpec())
			requireKind(t, err, machine.ErrInvalid)
			e.requireNoMachine(t, tt.id)
			assert.Zero(t, e.disks.Calls("create-root"), "no disk was made")
			assert.False(t, e.net.Allocated(tt.id), "no address was taken")

			for _, ok := range accepted {
				e := newEnvWithRange(machine.UIDRange{First: 2000, Count: 1})
				require.NoError(t, e.svc.Create(ctx, ok, "r", baseSpec()), "control %q", ok)
			}
			require.NoError(t, e.svc.Create(ctx, "a.b_C-1", "r", baseSpec()), "the refusal took no UID")
		})
	}
}

// Row "Restore", column Needs: the same form.
func TestRestoreSnapshot_MachineIdentifierForm(t *testing.T) {
	ctx := context.Background()
	for _, tt := range unusableIDs {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnvWithRangeAndCaps(machine.UIDRange{First: 2000, Count: 2}, snapshotCaps...)
			e.snapshotted(t, "src", machine.StateRunning)
			err := e.svc.RestoreSnapshot(ctx, tt.id, "r", "snap", narrowPolicy)
			requireKind(t, err, machine.ErrInvalid)
			e.requireNoMachine(t, tt.id)
			assert.Zero(t, e.disks.Calls("create-root-from"), "no disk was made")
			assert.False(t, e.net.Allocated(tt.id), "no address was taken")
			_, held := e.disks.Holds(machine.RootDiskID(tt.id))
			assert.False(t, held, "no root disk of the identifier is held")

			require.NoError(t, e.svc.RestoreSnapshot(ctx, "a.b_C-1", "r", "snap", narrowPolicy), "the refusal took no UID")
			e2 := newEnvWithRangeAndCaps(machine.UIDRange{First: 2000, Count: 2}, snapshotCaps...)
			e2.snapshotted(t, "src", machine.StateRunning)
			require.NoError(t, e2.svc.RestoreSnapshot(ctx, longest, "r", "snap", narrowPolicy), "the longest identifier that fits")
		})
	}
}
