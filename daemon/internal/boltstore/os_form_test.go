package boltstore_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// An OS-form Machine's boot set and command line are stored with its record and
// with the request that created it, and read back after a restart; a Machine of
// the image form reads back with none (ADR 0058).
func TestStore_KeepsTheOSForm(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)

	os := pausedMachine()
	os.OS = &machine.OSBoot{BootSet: "os", CommandLine: "console=ttyS0"}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "m", Machine: os, Request: machine.Request{ID: "r1", Spec: os.Spec}}))
	image := pausedMachine()
	image.ID = "image"
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "image", Machine: image}))
	require.NoError(t, db.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	byID := map[machine.MachineID]machine.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	want := &machine.OSBoot{BootSet: "os", CommandLine: "console=ttyS0"}
	assert.Equal(t, want, byID["m"].Machine.OS)
	assert.Equal(t, want, byID["m"].Request.Spec.OS)
	assert.Nil(t, byID["image"].Machine.OS)
}
