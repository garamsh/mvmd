package machine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const oldMarker = "OLD-VALUE-77aa03"

// stoppedWithRun creates a Machine with one env var and one file holding
// oldMarker, starts it once and stops it.
func (e *env) stoppedWithRun(t *testing.T, id machine.MachineID) {
	t.Helper()
	e.withRun(image, machine.ImageRun{Entrypoint: []string{"/ep"}, Env: map[string]string{"IMAGE": "kept"}})
	e.startedWith(t, id, image, func(s *machine.Spec) {
		s.Env = []machine.EnvVar{{Name: "OLD", Value: oldMarker}}
		s.Files = []machine.PlacedFile{{Path: "/etc/old", Content: []byte(oldMarker), Mode: 0o600}}
	})
	require.NoError(t, e.svc.Stop(context.Background(), id, time.Second))
}

func newRun() machine.RunUpdate {
	return machine.RunUpdate{
		Env:   []machine.EnvVar{{Name: "NEW", Value: marker}},
		Files: []machine.PlacedFile{{Path: "/etc/new", Content: []byte(marker), Mode: 0o640, UID: 5, GID: 6}},
	}
}

// requireRecordHolds reads the State Store, which is what a restarted daemon
// reloads.
func (e *env) requireRecordHolds(t *testing.T, id machine.MachineID, env []machine.EnvVar, files []machine.PlacedFile) {
	t.Helper()
	rec, ok := e.store.Record(id)
	require.True(t, ok)
	assert.Equal(t, env, rec.Machine.Env)
	assert.Equal(t, files, rec.Machine.Files)
}

func manyFiles(n int) []machine.PlacedFile {
	out := make([]machine.PlacedFile, n)
	for i := range out {
		out[i] = machine.PlacedFile{Path: "/data/f" + strings.Repeat("x", i+1), Content: []byte(marker), Mode: 0o600}
	}
	return out
}

// A start with a run update boots with its env and files in place of the
// record's, the record keeps them, and a later start without one boots with
// them again.
func TestStartWith_BootsWithTheNewEnvAndFilesAndKeepsThem(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.stoppedWithRun(t, "m")
	update := newRun()

	require.NoError(t, e.svc.StartWith(ctx, "m", update))
	boots := e.guest.Boots("m")
	require.Len(t, boots, 2)
	assert.Equal(t, map[string]string{"IMAGE": "kept", "NEW": marker}, boots[1].Run.Env, "the new env over the image's, the old one gone")
	assert.Equal(t, update.Files, boots[1].Files, "the new files, the old ones gone")
	e.requireRecordHolds(t, "m", update.Env, update.Files)

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.Start(ctx, "m"))
	boots = e.guest.Boots("m")
	require.Len(t, boots, 3)
	assert.Equal(t, boots[1].Run, boots[2].Run, "no update: the record's, which the update replaced")
	assert.Equal(t, update.Files, boots[2].Files)

	svc := e.reopened(t)
	require.NoError(t, svc.Stop(ctx, "m", time.Second))
	require.NoError(t, svc.Start(ctx, "m"))
	assert.Equal(t, update.Files, e.guest.Boots("m")[3].Files, "after the daemon reopened the store")
}

// Control for the test above: with no update a start boots with the record's.
func TestStart_WithoutAnUpdateBootsWithTheRecordsEnvAndFiles(t *testing.T) {
	e := newEnv()
	e.stoppedWithRun(t, "m")
	require.NoError(t, e.svc.Start(context.Background(), "m"))
	boots := e.guest.Boots("m")
	require.Len(t, boots, 2)
	assert.Equal(t, map[string]string{"IMAGE": "kept", "OLD": oldMarker}, boots[1].Run.Env)
	require.Len(t, boots[1].Files, 1)
	assert.Equal(t, "/etc/old", boots[1].Files[0].Path)
}

// A present update with both lists empty clears both.
func TestStartWith_EmptyListsClearTheEnvAndFiles(t *testing.T) {
	e := newEnv()
	e.stoppedWithRun(t, "m")
	require.NoError(t, e.svc.StartWith(context.Background(), "m", machine.RunUpdate{}))
	boots := e.guest.Boots("m")
	require.Len(t, boots, 2)
	assert.Equal(t, map[string]string{"IMAGE": "kept"}, boots[1].Run.Env)
	assert.Empty(t, boots[1].Files)
	e.requireRecordHolds(t, "m", nil, nil)
}

// A Machine that was never started takes the update at its first start.
func TestStartWith_ACreatedMachineBootsWithTheUpdate(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	spec := baseSpec()
	spec.Env = []machine.EnvVar{{Name: "OLD", Value: oldMarker}}
	require.NoError(t, e.svc.Create(ctx, "m", "r", spec))
	require.NoError(t, e.svc.StartWith(ctx, "m", newRun()))
	boots := e.guest.Boots("m")
	require.Len(t, boots, 1)
	assert.Equal(t, marker, boots[0].Run.Env["NEW"])
	assert.NotContains(t, boots[0].Run.Env, "OLD")
}

// A refused update leaves the record and the Machine as they were, names the
// field, and carries neither value nor content.
func TestStartWith_ARefusedUpdateChangesNothingAndEchoesNoSecret(t *testing.T) {
	file := func(path string) machine.PlacedFile {
		return machine.PlacedFile{Path: path, Content: []byte(marker), Mode: 0o600}
	}
	files := func(fs ...machine.PlacedFile) machine.RunUpdate { return machine.RunUpdate{Files: fs} }
	tests := []struct {
		name   string
		update machine.RunUpdate
		field  string
	}{
		{"empty env name", machine.RunUpdate{Env: []machine.EnvVar{{Value: marker}}}, "env"},
		{"env name with =", machine.RunUpdate{Env: []machine.EnvVar{{Name: "A=B", Value: marker}}}, "env"},
		{"env name twice", machine.RunUpdate{Env: []machine.EnvVar{{Name: "A", Value: marker}, {Name: "A", Value: marker}}}, "env"},
		{"relative path", files(file("etc/x")), "files"},
		{"under /proc", files(file("/proc/x")), "files"},
		{"under /run/mvmd", files(file("/run/mvmd/x")), "files"},
		{"mode above 07777", files(machine.PlacedFile{Path: "/etc/x", Content: []byte(marker), Mode: 0o10000}), "files"},
		{"path twice", files(file("/etc/x"), file("/etc/x")), "files"},
		{"65 files", files(manyFiles(65)...), "files"},
		{"over 1 MiB", files(machine.PlacedFile{Path: "/etc/x", Content: []byte(strings.Repeat("a", 1<<20+1)), Mode: 0o600}), "files"},
		{"a good env with a bad file", machine.RunUpdate{Env: newRun().Env, Files: []machine.PlacedFile{file("/sys/x")}}, "files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv()
			e.stoppedWithRun(t, "m")
			before, _ := e.store.Record("m")

			err := e.svc.StartWith(ctx, "m", tt.update)
			requireKind(t, err, machine.ErrInvalid)
			assert.Contains(t, err.Error(), tt.field)
			assert.NotContains(t, err.Error(), marker, "no value or content in an error")
			e.requireState(t, "m", machine.StateStopped)
			after, _ := e.store.Record("m")
			assert.Equal(t, before.Machine, after.Machine, "the record is unchanged")
			assert.Len(t, e.guest.Boots("m"), 1, "nothing booted")

			// Control: the valid update is accepted by the same Machine, so the
			// refusal was the update's and not the Machine's.
			require.NoError(t, e.svc.StartWith(ctx, "m", newRun()))
			e.requireState(t, "m", machine.StateRunning)
		})
	}

	t.Run("a refusal of a path names the path", func(t *testing.T) {
		e := newEnv()
		e.stoppedWithRun(t, "m")
		err := e.svc.StartWith(context.Background(), "m", files(file("/proc/the-path")))
		requireKind(t, err, machine.ErrInvalid)
		assert.Contains(t, err.Error(), "/proc/the-path")
	})
}

// The limits themselves are accepted at a start, as at Create.
func TestStartWith_TheLimitsThemselvesAreAccepted(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.stoppedWithRun(t, "m")
	require.NoError(t, e.svc.StartWith(ctx, "m", machine.RunUpdate{Files: manyFiles(64)}))
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	big := machine.PlacedFile{Path: "/etc/x", Content: []byte(strings.Repeat("a", 1<<20)), Mode: 0o7777}
	require.NoError(t, e.svc.StartWith(ctx, "m", machine.RunUpdate{Files: []machine.PlacedFile{big}}))
}

// A boot that fails after the update was recorded keeps the new values, and
// the next start without an update boots with them.
func TestStartWith_AFailedBootKeepsTheNewValuesForTheNextStart(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.stoppedWithRun(t, "m")
	update := newRun()

	e.guest.FailAt("boot", errBoom)
	require.Error(t, e.svc.StartWith(ctx, "m", update))
	e.requireRecordHolds(t, "m", update.Env, update.Files)

	e.guest.Heal("boot")
	m, err := e.svc.Inspect(ctx, "m")
	require.NoError(t, err)
	if m.State != machine.StateStopped {
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	}
	require.NoError(t, e.svc.Start(ctx, "m"))
	boots := e.guest.Boots("m")
	require.NotEmpty(t, boots)
	last := boots[len(boots)-1]
	assert.Equal(t, update.Files, last.Files)
	assert.Equal(t, marker, last.Run.Env["NEW"])
}

// A start that no check lets reach the boot does not write the update.
func TestStartWith_AStartThatIsNotPermittedLeavesTheRecordAlone(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.stoppedWithRun(t, "m")
	before, _ := e.store.Record("m")

	e.permit.FailAt("permit", errBoom)
	require.Error(t, e.svc.StartWith(ctx, "m", newRun()))
	after, _ := e.store.Record("m")
	assert.Equal(t, before.Machine, after.Machine)

	e.permit.Heal("permit")
	require.NoError(t, e.svc.StartWith(ctx, "m", newRun()), "control: permitted, the same update boots")
}

// A Running Machine takes no new env or files, and a repeat of the start that
// booted it succeeds.
func TestStartWith_ARunningMachineAcceptsOnlyWhatItHolds(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.stoppedWithRun(t, "m")
	update := newRun()
	require.NoError(t, e.svc.StartWith(ctx, "m", update))

	require.NoError(t, e.svc.StartWith(ctx, "m", update), "a repeat of the same start")
	assert.Len(t, e.guest.Boots("m"), 2, "it booted nothing")

	other := machine.RunUpdate{Env: []machine.EnvVar{{Name: "OTHER", Value: marker}}}
	err := e.svc.StartWith(ctx, "m", other)
	requireKind(t, err, machine.ErrPreconditionFailed)
	assert.NotContains(t, err.Error(), marker)
	e.requireRecordHolds(t, "m", update.Env, update.Files)
}
