package boltstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// restart_on_boot, the boot a start began in and the mark of a pending restart
// are stored with the record, read back after a restart, and absent from the
// record of a Machine that has none (ADR 0062).
func TestStore_KeepsTheRestartOnBootFacts(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)

	set := pausedMachine()
	set.RestartOnBoot, set.StartedInBoot, set.RestartPending = true, "boot-1", true
	request := machine.Request{ID: "r1", Spec: set.Spec}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "m", Machine: set, Request: request}))
	none := pausedMachine()
	none.ID = "none"
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "none", Machine: none}))
	require.NoError(t, db.Close())

	record, _ := storedBytes(t, dir, "m")
	assert.Equal(t, 2, strings.Count(record, `"RestartOnBoot":true`), "in the Machine and in its request")
	assert.Contains(t, record, `"StartedInBoot":"boot-1"`)
	assert.Contains(t, record, `"RestartPending":true`)
	control, _ := storedBytes(t, dir, "none")
	assert.NotContains(t, control, "RestartOnBoot")
	assert.NotContains(t, control, "StartedInBoot")
	assert.NotContains(t, control, "RestartPending")

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	byID := map[machine.MachineID]machine.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	m := byID["m"].Machine
	assert.True(t, m.RestartOnBoot)
	assert.Equal(t, "boot-1", m.StartedInBoot)
	assert.True(t, m.RestartPending)
	assert.True(t, byID["m"].Request.Spec.RestartOnBoot)
	assert.False(t, byID["none"].Machine.RestartOnBoot)
}
