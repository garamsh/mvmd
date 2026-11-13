package boltstore_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A Machine's probes are stored with its record and with the request that
// created it, read back after a restart with every field and in order, and are
// absent from the record of a Machine that had none.
func TestStore_KeepsTheProbes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)

	probes := []machine.Probe{
		{Name: "ready", Kind: machine.ProbeExec, Argv: []string{"/bin/check", "-q"}, InitialDelay: 2 * time.Second, Period: 5 * time.Second, Timeout: 500 * time.Millisecond, SuccessThreshold: 2, FailureThreshold: 4},
		{Name: "port", Kind: machine.ProbeTCP, Port: 8080},
		{Name: "web", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/healthz", Scheme: "http"},
	}
	set := pausedMachine()
	set.Probes = probes
	request := machine.Request{ID: "r1", Spec: set.Spec}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "m", Machine: set, Request: request}))
	none := pausedMachine()
	none.ID = "none"
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "none", Machine: none}))
	require.NoError(t, db.Close())

	record, _ := storedBytes(t, dir, "m")
	assert.Equal(t, 2, strings.Count(record, `"Name":"ready"`), "in the Machine and in its request")
	control, _ := storedBytes(t, dir, "none")
	assert.NotContains(t, control, "Probes")

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	byID := map[machine.MachineID]machine.Record{}
	for _, r := range got {
		byID[r.ID] = r
	}
	assert.Equal(t, probes, byID["m"].Machine.Probes)
	assert.Equal(t, probes, byID["m"].Request.Spec.Probes)
	assert.Empty(t, byID["none"].Machine.Probes)
}
