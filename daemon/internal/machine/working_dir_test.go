package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The Machine's working directory replaces the image's, an empty one keeps the
// image's, and every boot sends the same.
func TestStart_WorkingDirectoryIsTheMachinesElseTheImages(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.withRun("sha256:wd", machine.ImageRun{Entrypoint: []string{"/ep"}, WorkingDir: "/image-dir"})

	boot := e.startedWith(t, "set", "sha256:wd", func(s *machine.Spec) { s.WorkingDir = "/work" })
	assert.Equal(t, "/work", boot.Run.WorkingDir)
	assert.Equal(t, "/work", e.requireState(t, "set", machine.StateRunning).WorkingDir, "Inspect reports it")

	require.NoError(t, e.svc.Stop(ctx, "set", time.Second))
	require.NoError(t, e.svc.Start(ctx, "set"))
	boots := e.guest.Boots("set")
	require.Len(t, boots, 2)
	assert.Equal(t, "/work", boots[1].Run.WorkingDir, "the boot after a Stop")

	// Control: none set runs in the image's.
	other := e.startedWith(t, "none", "sha256:wd", func(*machine.Spec) {})
	assert.Equal(t, "/image-dir", other.Run.WorkingDir)
	assert.Empty(t, e.requireState(t, "none", machine.StateRunning).WorkingDir, "the Machine holds none of its own")
}

func TestCreate_AWorkingDirectoryThatIsNotAbsoluteAndCleanIsRefused(t *testing.T) {
	for name, dir := range map[string]string{
		"relative":       "work",
		"dot element":    "/a/./b",
		"dotdot element": "/a/../b",
		"trailing slash": "/work/",
		"empty element":  "/a//b",
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv()
			spec := baseSpec()
			spec.WorkingDir = dir
			err := e.svc.Create(ctx, "m", "r", spec)
			requireKind(t, err, machine.ErrInvalid)
			assert.Contains(t, err.Error(), "working_dir")
			assert.Contains(t, err.Error(), dir, "it names the path")
			e.requireNoMachine(t, "m")

			// Control: an absolute and clean directory is accepted, the root included.
			spec.WorkingDir = "/work"
			require.NoError(t, e.svc.Create(ctx, "a", "ra", spec))
			spec.WorkingDir = "/"
			require.NoError(t, e.svc.Create(ctx, "b", "rb", spec))
		})
	}
}

// A restored Machine that is stopped and started again runs in its source's
// working directory, and one whose source set none runs in the image's.
func TestRestoreSnapshot_BootsLaterInTheSourcesWorkingDirectory(t *testing.T) {
	ctx := context.Background()
	run := machine.ImageRun{Entrypoint: []string{"/ep"}, WorkingDir: "/image-dir"}

	for _, tt := range []struct{ name, set, want string }{
		{"the source set one", "/work", "/work"},
		{"control: the source set none", "", "/image-dir"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.withRun(image, run)
			e.snapshottedWith(t, "src", func(s *machine.Spec) { s.WorkingDir = tt.set })
			got, _, err := e.snaps.Get(ctx, "snap")
			require.NoError(t, err)
			require.Equal(t, tt.set, got.Spec.WorkingDir, "the setup: the snapshot kept it")

			require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
			require.NoError(t, e.svc.Stop(ctx, "new", time.Second))
			require.NoError(t, e.svc.Start(ctx, "new"))
			boots := e.guest.Boots("new")
			require.Len(t, boots, 1)
			assert.Equal(t, tt.want, boots[0].Run.WorkingDir)
		})
	}
}
