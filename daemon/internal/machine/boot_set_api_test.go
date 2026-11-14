package machine_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// bytesUpload is the two files of a set with the digests of their bytes.
func bytesUpload(kernel, initrd string) machine.BootUpload {
	return machine.BootUpload{
		Kernel: machine.BootContent{SHA256: hexSum(kernel), Content: bytes.NewReader([]byte(kernel))},
		Initrd: machine.BootContent{SHA256: hexSum(initrd), Content: bytes.NewReader([]byte(initrd))},
	}
}

func hexSum(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// addEnv is an osEnv whose node also keeps the boot sets a client adds.
func addEnv(t *testing.T) (*env, *memory.BootFiles) {
	t.Helper()
	e := newEnv()
	files := &memory.BootFiles{}
	e.svc = e.reopenedWith(t, machine.WithBootSets(memory.BootSets(), files), machine.WithAddedBootSets(memory.NewBootSetStore()))
	return e, files
}

func addedSpec(name string) machine.Spec {
	spec := osSpec()
	spec.OS.BootSet = name
	return spec
}

func TestAddBootSet_AddsASetListedWithItsDigestsAndOrigin(t *testing.T) {
	ctx := context.Background()
	e, _ := addEnv(t)

	info, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
	require.NoError(t, err)
	assert.Equal(t, machine.BootSetAdded, info.Origin)
	assert.Equal(t, hexSum("kernel-a"), info.Set.Kernel.SHA256)
	assert.Equal(t, hexSum("initrd-a"), info.Set.Initrd.SHA256)

	listed, err := e.svc.ListBootSets(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 2)
	assert.Equal(t, "mine", listed[0].Name, "by name: m before o")
	assert.Equal(t, machine.BootSetAdded, listed[0].Origin)
	assert.Equal(t, hexSum("kernel-a"), listed[0].Set.Kernel.SHA256)
	assert.Equal(t, "os", listed[1].Name)
	assert.Equal(t, machine.BootSetConfigured, listed[1].Origin, "control: the configured set is listed as configured")
}

func TestAddBootSet_RefusesAFileWhoseBytesAreNotItsDigestAndNamesIt(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		file   string
		change func(*machine.BootUpload)
	}{
		{"kernel", func(u *machine.BootUpload) { u.Kernel.SHA256 = hexSum("another kernel") }},
		{"initrd", func(u *machine.BootUpload) { u.Initrd.SHA256 = hexSum("another initrd") }},
	} {
		t.Run(tc.file, func(t *testing.T) {
			e, _ := addEnv(t)
			upload := bytesUpload("kernel-a", "initrd-a")
			tc.change(&upload)
			_, err := e.svc.AddBootSet(ctx, "mine", upload)
			requireKind(t, err, machine.ErrInvalid)
			assert.ErrorContains(t, err, tc.file)

			listed, err := e.svc.ListBootSets(ctx)
			require.NoError(t, err)
			assert.Len(t, listed, 1, "nothing was published")
			_, err = e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
			require.NoError(t, err, "control: the same files with their own digests are accepted")
		})
	}
}

func TestAddBootSet_RefusesAMalformedNameOrDigest(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		field string
		name  string
		edit  func(*machine.BootUpload)
	}{
		{"name", "Not Valid", func(*machine.BootUpload) {}},
		{"name", "", func(*machine.BootUpload) {}},
		{"kernel digest", "mine", func(u *machine.BootUpload) { u.Kernel.SHA256 = "sha256:abc" }},
		{"initrd digest", "mine", func(u *machine.BootUpload) { u.Initrd.SHA256 = "" }},
	} {
		t.Run(tc.field+" "+tc.name, func(t *testing.T) {
			e, _ := addEnv(t)
			upload := bytesUpload("k", "i")
			tc.edit(&upload)
			_, err := e.svc.AddBootSet(ctx, tc.name, upload)
			requireKind(t, err, machine.ErrInvalid)
			assert.ErrorContains(t, err, tc.field)
			_, err = e.svc.AddBootSet(ctx, "mine", bytesUpload("k", "i"))
			require.NoError(t, err, "control: a well-formed add is accepted")
		})
	}
}

func TestAddBootSet_ATakenName(t *testing.T) {
	ctx := context.Background()

	t.Run("with the same digests is success and changes nothing", func(t *testing.T) {
		e, _ := addEnv(t)
		first, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
		require.NoError(t, err)
		again, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
		require.NoError(t, err)
		assert.Equal(t, first, again)
		listed, err := e.svc.ListBootSets(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 2)
	})

	t.Run("with other digests is a conflict and keeps the set", func(t *testing.T) {
		e, _ := addEnv(t)
		first, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
		require.NoError(t, err)
		_, err = e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-b", "initrd-a"))
		requireKind(t, err, machine.ErrConflict)
		_, err = e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-b"))
		requireKind(t, err, machine.ErrConflict)
		listed, err := e.svc.ListBootSets(ctx)
		require.NoError(t, err)
		assert.Equal(t, first, listed[0], "the set under the name is the first one")
	})

	t.Run("a configured name is not taken over", func(t *testing.T) {
		e, _ := addEnv(t)
		_, err := e.svc.AddBootSet(ctx, "os", bytesUpload("kernel-a", "initrd-a"))
		requireKind(t, err, machine.ErrConflict)
		_, err = e.svc.AddBootSet(ctx, "os2", bytesUpload("kernel-a", "initrd-a"))
		require.NoError(t, err, "control: a free name is accepted")
	})
}

func TestAddBootSet_ANodeThatKeepsNoAddedSetRefuses(t *testing.T) {
	e, _ := osEnv(t)
	_, err := e.svc.AddBootSet(context.Background(), "mine", bytesUpload("k", "i"))
	requireKind(t, err, machine.ErrUnsupported)
}

func TestAddedBootSet_ARunnableMachineIsCreatedAndStartedOnIt(t *testing.T) {
	ctx := context.Background()
	e, _ := addEnv(t)

	err := e.svc.Create(ctx, "m", request("m"), addedSpec("mine"))
	requireKind(t, err, machine.ErrInvalid)
	assert.ErrorContains(t, err, "mine")

	_, err = e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
	require.NoError(t, err)
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), addedSpec("mine")))
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.requireState(t, "m", machine.StateRunning)
}

func TestStart_AnAddedSetsFileChangedAfterTheAddFailsTheCheck(t *testing.T) {
	ctx := context.Background()
	e, files := addEnv(t)
	info, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
	require.NoError(t, err)
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), addedSpec("mine")))

	files.Change(info.Set.Initrd.Path)
	err = e.svc.Start(ctx, "m")
	require.Error(t, err)
	assert.ErrorContains(t, err, info.Set.Initrd.Path)
	e.requireState(t, "m", machine.StateCreated)
}

func TestRemoveBootSet(t *testing.T) {
	ctx := context.Background()

	t.Run("succeeds for a set no Machine names", func(t *testing.T) {
		e, _ := addEnv(t)
		_, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
		require.NoError(t, err)
		require.NoError(t, e.svc.RemoveBootSet(ctx, "mine"))
		listed, err := e.svc.ListBootSets(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 1)
		require.NoError(t, e.svc.RemoveBootSet(ctx, "mine"), "an absent name is success")
	})

	t.Run("is refused while a Machine's record names the set and succeeds once none does", func(t *testing.T) {
		for _, start := range []bool{false, true} {
			e, _ := addEnv(t)
			_, err := e.svc.AddBootSet(ctx, "mine", bytesUpload("kernel-a", "initrd-a"))
			require.NoError(t, err)
			_, err = e.svc.AddBootSet(ctx, "other", bytesUpload("kernel-b", "initrd-b"))
			require.NoError(t, err)
			require.NoError(t, e.svc.Create(ctx, "m", request("m"), addedSpec("mine")))
			if start {
				require.NoError(t, e.svc.Start(ctx, "m"))
			}

			require.NoError(t, e.svc.RemoveBootSet(ctx, "other"), "the near miss: a set no Machine names goes")
			err = e.svc.RemoveBootSet(ctx, "mine")
			requireKind(t, err, machine.ErrPreconditionFailed)
			assert.ErrorContains(t, err, `"m"`)
			listed, err := e.svc.ListBootSets(ctx)
			require.NoError(t, err)
			require.Len(t, listed, 2, "the set stays")

			e.deleted(t, "m")
			require.NoError(t, e.svc.RemoveBootSet(ctx, "mine"))
		}
	})

	t.Run("refuses a configured set", func(t *testing.T) {
		e, _ := addEnv(t)
		err := e.svc.RemoveBootSet(ctx, "os")
		requireKind(t, err, machine.ErrPreconditionFailed)
		listed, err := e.svc.ListBootSets(ctx)
		require.NoError(t, err)
		assert.Len(t, listed, 1, "the configured set stays")
	})
}

// holdingStore holds a Get of the armed name until released, so a call is
// inside its resolution of the name.
type holdingStore struct {
	machine.BootSetStore
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (h *holdingStore) Get(ctx context.Context, name string) (machine.BootSet, error) {
	set, err := h.BootSetStore.Get(ctx, name)
	h.mu.Lock()
	armed := h.armed
	h.armed = false
	h.mu.Unlock()
	if armed {
		close(h.entered)
		<-h.release
	}
	return set, err
}

// Row "Remove boot set", column Needs: no Create is resolving the name.
func TestRemoveBootSet_RefusesASetACreateIsResolving(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	store := &holdingStore{BootSetStore: memory.NewBootSetStore(), entered: make(chan struct{}), release: make(chan struct{})}
	e.svc = e.reopenedWith(t, machine.WithBootSets(memory.BootSets(), &memory.BootFiles{}), machine.WithAddedBootSets(store))
	for _, name := range []string{"mine", "other"} {
		_, err := e.svc.AddBootSet(ctx, name, bytesUpload("kernel-"+name, "initrd-"+name))
		require.NoError(t, err)
	}

	store.armed = true
	creating := make(chan error, 1)
	go func() { creating <- e.svc.Create(ctx, "m", request("m"), addedSpec("mine")) }()
	<-store.entered

	require.NoError(t, e.svc.RemoveBootSet(ctx, "other"), "the near miss: another set goes")
	err := e.svc.RemoveBootSet(ctx, "mine")
	close(store.release)

	requireKind(t, err, machine.ErrPreconditionFailed)
	require.NoError(t, <-creating)
	e.requireState(t, "m", machine.StateCreated)
	_, err = store.Get(ctx, "mine")
	require.NoError(t, err, "the set the Machine names is still held")
}
