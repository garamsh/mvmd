package machine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// withRunConfig is a spec that sets all five run fields; marker is the value of
// the env var and the content of the file.
func withRunConfig(s *machine.Spec) {
	s.Command, s.Args, s.User = []string{"/mine"}, []string{"x"}, "7:8"
	s.Env = []machine.EnvVar{{Name: "SECRET", Value: marker}}
	s.Files = []machine.PlacedFile{{Path: "/etc/s", Content: []byte(marker), Mode: 0o600, UID: 7, GID: 8}}
}

// snapshottedWith creates the Machine with the edit applied to the base spec,
// starts it and snapshots it as "snap".
func (e *env) snapshottedWith(t *testing.T, id machine.MachineID, edit func(*machine.Spec)) {
	t.Helper()
	ctx := context.Background()
	spec := baseSpec()
	edit(&spec)
	require.NoError(t, e.svc.Create(ctx, id, request(id), spec))
	require.NoError(t, e.svc.Start(ctx, id))
	require.NoError(t, e.svc.Snapshot(ctx, id, "snap", "r-snap"))
}

// A snapshot keeps the five fields of its Machine, and one of a Machine that
// set none keeps none.
func TestSnapshot_KeepsTheRunConfiguration(t *testing.T) {
	ctx := context.Background()

	e := newEnv(snapshotCaps...)
	e.snapshottedWith(t, "m", withRunConfig)
	got, _, err := e.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	assert.Equal(t, []string{"/mine"}, got.Spec.Command)
	assert.Equal(t, []string{"x"}, got.Spec.Args)
	assert.Equal(t, "7:8", got.Spec.User)
	assert.Equal(t, []machine.EnvVar{{Name: "SECRET", Value: marker}}, got.Spec.Env)
	assert.Equal(t, []machine.PlacedFile{{Path: "/etc/s", Content: []byte(marker), Mode: 0o600, UID: 7, GID: 8}}, got.Spec.Files)

	control := newEnv(snapshotCaps...)
	control.snapshottedWith(t, "m", func(*machine.Spec) {})
	got, _, err = control.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	assert.Empty(t, got.Spec.Command)
	assert.Empty(t, got.Spec.Args)
	assert.Empty(t, got.Spec.User)
	assert.Empty(t, got.Spec.Env)
	assert.Empty(t, got.Spec.Files)
}

// A Machine restored from a snapshot, stopped and started again, boots with
// the command, args, environment, user and files of the Machine the snapshot
// came from.
func TestRestoreSnapshot_BootsLaterWithTheSourcesRunConfiguration(t *testing.T) {
	ctx := context.Background()
	run := machine.ImageRun{Entrypoint: []string{"/ep"}, Cmd: []string{"cmd"}, Env: map[string]string{"KEEP": "image"}, User: "app"}

	e := newEnv(snapshotCaps...)
	e.withRun(image, run)
	e.snapshottedWith(t, "src", withRunConfig)
	source := e.guest.Boots("src")
	require.Len(t, source, 1)
	require.Equal(t, []string{"/mine", "x"}, source[0].Run.Command, "the setup: the source booted with its run configuration")

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	require.NoError(t, e.svc.Stop(ctx, "new", time.Second))
	require.NoError(t, e.svc.Start(ctx, "new"))

	boots := e.guest.Boots("new")
	require.Len(t, boots, 1, "the restore sends no boot, the start sends one")
	assert.Equal(t, source[0].Run, boots[0].Run)
	assert.Equal(t, "7:8", boots[0].User)
	assert.Equal(t, source[0].Files, boots[0].Files)
	assert.Equal(t, map[string]string{"KEEP": "image", "SECRET": marker}, boots[0].Run.Env)

	// Control: a source with none set restores with none set, and boots with
	// the image's own.
	c := newEnv(snapshotCaps...)
	c.withRun(image, run)
	c.snapshottedWith(t, "src", func(*machine.Spec) {})
	require.NoError(t, c.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	require.NoError(t, c.svc.Stop(ctx, "new", time.Second))
	require.NoError(t, c.svc.Start(ctx, "new"))
	boots = c.guest.Boots("new")
	require.Len(t, boots, 1)
	assert.Equal(t, []string{"/ep", "cmd"}, boots[0].Run.Command)
	assert.Equal(t, map[string]string{"KEEP": "image"}, boots[0].Run.Env)
	assert.Equal(t, "app", boots[0].User)
	assert.Empty(t, boots[0].Files)
}

// A restore records the fields in the new Machine's own record, so they
// outlive the daemon, and a restore does not share them with its source.
func TestRestoreSnapshot_RecordsTheRunConfigurationInTheNewMachine(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshottedWith(t, "src", withRunConfig)
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))

	rec, ok := e.store.Record("new")
	require.True(t, ok)
	assert.Equal(t, []string{"/mine"}, rec.Machine.Command)
	assert.Equal(t, "7:8", rec.Machine.Spec.User)
	assert.Equal(t, []machine.EnvVar{{Name: "SECRET", Value: marker}}, rec.Machine.Env)
	require.Len(t, rec.Machine.Files, 1)
	assert.Equal(t, []byte(marker), rec.Machine.Files[0].Content)

	rec.Machine.Files[0].Content[0] ^= 0xff
	held, _, err := e.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	assert.Equal(t, []byte(marker), held.Spec.Files[0].Content, "a change of the new Machine's content does not reach the snapshot")
}

// A snapshot written before the fields were kept has none, and its restore
// boots with the image's run.
func TestRestoreSnapshot_ASnapshotWithoutTheFieldsBootsWithTheImagesRun(t *testing.T) {
	ctx := context.Background()
	run := machine.ImageRun{Entrypoint: []string{"/ep"}, Cmd: []string{"cmd"}, User: "app"}
	e := newEnv(snapshotCaps...)
	e.withRun(image, run)
	e.snapshottedWith(t, "src", withRunConfig)

	held, files, err := e.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	require.NotEmpty(t, held.Spec.Command, "the setup: the snapshot holds the fields")
	held.ID = "old"
	held.Spec.Command, held.Spec.Args, held.Spec.Env, held.Spec.User, held.Spec.Files = nil, nil, nil, "", nil
	require.NoError(t, e.snaps.Put(ctx, held, files))

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "old", narrowPolicy))
	require.NoError(t, e.svc.Stop(ctx, "new", time.Second))
	require.NoError(t, e.svc.Start(ctx, "new"))
	boots := e.guest.Boots("new")
	require.Len(t, boots, 1)
	assert.Equal(t, []string{"/ep", "cmd"}, boots[0].Run.Command)
	assert.Equal(t, "app", boots[0].User)
	assert.Empty(t, boots[0].Files)
}

// No listing, and no error of a snapshot or a restore, holds an env value or a
// file's content.
func TestSnapshot_NoValueOrContentIsListedOrInAnError(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshottedWith(t, "src", withRunConfig)

	listed, err := e.svc.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.NotContains(t, fmt.Sprintf("%+v", listed), marker)
	assert.Empty(t, listed[0].Spec.Files[0].Content, "a byte slice prints as numbers, so the content is read as it is")
	assert.Equal(t, "SECRET", listed[0].Spec.Env[0].Name, "control: the listing names the variable")
	assert.Equal(t, "/etc/s", listed[0].Spec.Files[0].Path, "control: and the file")

	e.snaps.FailAt("put", errBoom)
	err = e.svc.Snapshot(ctx, "src", "snap-2", "r-snap-2")
	requireKind(t, err, machine.ErrFailed)
	assert.NotContains(t, err.Error(), marker)
	e.snaps.Heal("put")

	e.guest.FailAt("catch-up", errBoom)
	err = e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
	requireKind(t, err, machine.ErrFailed)
	assert.NotContains(t, err.Error(), marker)
}
