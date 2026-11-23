package catalog_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/catalog"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

func upload(kernel, initrd string) machine.BootUpload {
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	return machine.BootUpload{
		Kernel: machine.BootContent{SHA256: sum(kernel), Content: bytes.NewReader([]byte(kernel))},
		Initrd: machine.BootContent{SHA256: sum(initrd), Content: bytes.NewReader([]byte(initrd))},
	}
}

func bootSetNode(t *testing.T) *node {
	t.Helper()
	n := newNode(t)
	n.catalog = catalog.NewService(n.images, n.snapshots, n.store, catalog.WithBootSets(memory.BootSets()), catalog.WithAddedBootSets(memory.NewBootSetStore()))
	return n
}

func kind(t *testing.T, err error) error {
	t.Helper()
	var failure *machine.Error
	require.ErrorAs(t, err, &failure)
	return failure.Kind
}

func TestBootSets_AnAddedSetIsResolvedAndListedWithItsOrigin(t *testing.T) {
	ctx := context.Background()
	n := bootSetNode(t)
	_, err := n.catalog.AddBootSet(ctx, "mine", upload("kernel", "initrd"))
	require.NoError(t, err)

	listed, err := n.catalog.ListBootSets(ctx)
	require.NoError(t, err)
	origins := map[string]machine.BootSetOrigin{}
	sets := map[string]machine.BootSet{}
	var names []string
	for _, info := range listed {
		origins[info.Name], sets[info.Name] = info.Origin, info.Set
		names = append(names, info.Name)
	}
	assert.Equal(t, []string{"mine", "os"}, names, "ordered by name")
	assert.Equal(t, machine.BootSetConfigured, origins["os"], "the configured set is listed with its origin")
	assert.Equal(t, machine.BootSetAdded, origins["mine"])

	set, err := n.catalog.ResolveBootSet(ctx, "mine")
	require.NoError(t, err)
	assert.Equal(t, sets["mine"], set)
	_, err = n.catalog.ResolveBootSet(ctx, "never-offered")
	assert.ErrorIs(t, err, machine.ErrBootSetAbsent)
}

func TestRemoveBootSet_RefusesASetAMachineRecordNamesAndALeaseHoldsOff(t *testing.T) {
	ctx := context.Background()
	n := bootSetNode(t)
	for _, name := range []string{"mine", "other", "leased"} {
		_, err := n.catalog.AddBootSet(ctx, name, upload("kernel-"+name, "initrd-"+name))
		require.NoError(t, err)
	}
	n.record(t, "m", machine.Record{Machine: &machine.Machine{ID: "m", State: machine.StateStopped, Spec: machine.Spec{OS: &machine.OSBoot{BootSet: "mine"}}}})

	require.NoError(t, n.catalog.RemoveBootSet(ctx, "other"), "the near miss: a set no Machine names goes")
	assert.ErrorIs(t, kind(t, n.catalog.RemoveBootSet(ctx, "mine")), machine.ErrPreconditionFailed)

	release := n.catalog.HoldBootSet("leased")
	assert.ErrorIs(t, kind(t, n.catalog.RemoveBootSet(ctx, "leased")), machine.ErrPreconditionFailed)
	release()
	require.NoError(t, n.catalog.RemoveBootSet(ctx, "leased"), "control: once the lease is released it goes")

	require.NoError(t, n.store.Remove(ctx, "m"))
	require.NoError(t, n.catalog.RemoveBootSet(ctx, "mine"), "once the record is gone nothing names the set")
}
