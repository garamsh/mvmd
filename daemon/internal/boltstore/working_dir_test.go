package boltstore_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A Machine's working directory is stored with its record and with the
// request that created it, reads back after a restart, and is absent from the
// record of a Machine that set none.
func TestStore_KeepsTheWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)

	set := pausedMachine()
	set.WorkingDir = "/srv/app"
	request := machine.Request{ID: "r1", Spec: set.Spec}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "m", Machine: set, Request: request}))
	none := pausedMachine()
	none.ID = "none"
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "none", Machine: none}))
	require.NoError(t, db.Close())

	record, _ := storedBytes(t, dir, "m")
	assert.Equal(t, 2, strings.Count(record, `"WorkingDir":"/srv/app"`), "in the Machine and in its request")
	control, _ := storedBytes(t, dir, "none")
	assert.NotContains(t, control, "WorkingDir")

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	byID := map[machine.MachineID]machine.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	assert.Equal(t, "/srv/app", byID["m"].Machine.WorkingDir)
	assert.Equal(t, "/srv/app", byID["m"].Request.Spec.WorkingDir)
	assert.Empty(t, byID["none"].Machine.WorkingDir)
}
