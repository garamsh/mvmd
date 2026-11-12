package machine_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// marker stands for a secret: it is the value of every env var and the
// content of every file the tests below give a Machine.
const marker = "MARKER-VALUE-4f9c1e"

func (e *env) withRun(digest machine.ImageDigest, run machine.ImageRun) {
	e.images.ProvideWith(digest, machine.LocatedImage{UUID: "uuid-" + string(digest), Run: run})
}

// startedWith creates the Machine on the image with the edit applied to the
// base spec, starts it, and returns what the guest was sent.
func (e *env) startedWith(t *testing.T, id machine.MachineID, digest machine.ImageDigest, edit func(*machine.Spec)) machine.GuestBoot {
	t.Helper()
	spec := baseSpec()
	spec.Image = digest
	edit(&spec)
	require.NoError(t, e.svc.Create(context.Background(), id, request(id), spec))
	require.NoError(t, e.svc.Start(context.Background(), id))
	boots := e.guest.Boots(id)
	require.Len(t, boots, 1, "the setup: the guest was sent one boot")
	return boots[0]
}

// Command and args replace the image's entrypoint and command the way
// Kubernetes does.
func TestStart_CommandAndArgsReplaceTheImagesTheWayKubernetesDoes(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		args    []string
		want    []string
	}{
		{"neither keeps both", nil, nil, []string{"/ep", "cmd"}},
		{"command drops the image command", []string{"/mine"}, nil, []string{"/mine"}},
		{"args alone keep the entrypoint", nil, []string{"x", "y"}, []string{"/ep", "x", "y"}},
		{"command and args replace both", []string{"/mine"}, []string{"x"}, []string{"/mine", "x"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			e.withRun("sha256:kube", machine.ImageRun{Entrypoint: []string{"/ep"}, Cmd: []string{"cmd"}})
			boot := e.startedWith(t, "m", "sha256:kube", func(s *machine.Spec) { s.Command, s.Args = tt.command, tt.args })
			assert.Equal(t, tt.want, boot.Run.Command)
		})
	}
}

// Env is applied over the image's environment, and the user replaces the
// image's only when the request sets one.
func TestStart_EnvIsAppliedOverTheImagesAndTheUserFallsBackToTheImages(t *testing.T) {
	e := newEnv()
	e.withRun("sha256:env", machine.ImageRun{Entrypoint: []string{"/ep"}, Env: map[string]string{"KEEP": "image", "OVER": "image"}, User: "app:staff"})

	boot := e.startedWith(t, "a", "sha256:env", func(s *machine.Spec) {
		s.Env = []machine.EnvVar{{Name: "OVER", Value: "request"}, {Name: "NEW", Value: "request"}}
	})
	assert.Equal(t, map[string]string{"KEEP": "image", "OVER": "request", "NEW": "request"}, boot.Run.Env)
	assert.Equal(t, "app:staff", boot.User, "no user in the request: the image's")

	// Control: a user in the request wins over the image's.
	other := e.startedWith(t, "b", "sha256:env", func(s *machine.Spec) { s.User = "1000:1000" })
	assert.Equal(t, "1000:1000", other.User)
	assert.Equal(t, map[string]string{"KEEP": "image", "OVER": "image"}, other.Run.Env, "no env in the request: the image's")

	// An image with no user runs as root, which is the empty user.
	e.withRun("sha256:root", machine.ImageRun{Entrypoint: []string{"/ep"}})
	assert.Empty(t, e.startedWith(t, "c", "sha256:root", func(*machine.Spec) {}).User)
}

// An image record holding one joined argument vector has no command for args
// alone to replace.
func TestCreate_ArgsAloneOnAnImageKeptJoinedIsRefused(t *testing.T) {
	e := newEnv()
	e.withRun("sha256:old", machine.ImageRun{Entrypoint: []string{"/ep", "cmd"}, Joined: true})
	e.withRun("sha256:new", machine.ImageRun{Entrypoint: []string{"/ep"}, Cmd: []string{"cmd"}})

	spec := baseSpec()
	spec.Image = "sha256:old"
	spec.Args = []string{"x"}
	err := e.svc.Create(context.Background(), "m", "r", spec)
	requireKind(t, err, machine.ErrPreconditionFailed)
	assert.Contains(t, err.Error(), "sha256:old", "it names the image")
	assert.Contains(t, err.Error(), "pull the image again")
	e.requireNoMachine(t, "m")

	// Controls: a command on the same image, and args on an image kept apart.
	spec.Args, spec.Command = nil, []string{"/mine"}
	require.NoError(t, e.svc.Create(context.Background(), "a", "ra", spec))
	spec.Image, spec.Command, spec.Args = "sha256:new", nil, []string{"x"}
	require.NoError(t, e.svc.Create(context.Background(), "b", "rb", spec))
}

// Every boot sends the same run configuration: a second Start, and a Start
// after the daemon reopened the store.
func TestStart_EveryBootSendsTheSameRunConfiguration(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	first := e.startedWith(t, "m", image, func(s *machine.Spec) {
		s.Command, s.Args, s.User = []string{"/mine"}, []string{"x"}, "7:8"
		s.Env = []machine.EnvVar{{Name: "SECRET", Value: marker}}
		s.Files = []machine.PlacedFile{{Path: "/etc/s", Content: []byte(marker), Mode: 0o600, UID: 7, GID: 8}}
	})
	require.Equal(t, []string{"/mine", "x"}, first.Run.Command, "the setup: the run configuration reached the guest")
	require.Len(t, first.Files, 1, "the setup: the files reached the guest")

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.Start(ctx, "m"))
	svc := e.reopened(t)
	require.NoError(t, svc.Stop(ctx, "m", time.Second))
	require.NoError(t, svc.Start(ctx, "m"))

	boots := e.guest.Boots("m")
	require.Len(t, boots, 3)
	for _, boot := range boots[1:] {
		assert.Equal(t, first.Run, boot.Run)
		assert.Equal(t, first.User, boot.User)
		assert.Equal(t, first.Files, boot.Files)
	}
}

// Row "Create Machine", column Needs: the run configuration.
func TestCreate_NeedsAValidRunConfiguration(t *testing.T) {
	file := func(path string) machine.PlacedFile {
		return machine.PlacedFile{Path: path, Content: []byte(marker), Mode: 0o600}
	}
	files := func(fs ...machine.PlacedFile) func(*machine.Spec) {
		return func(s *machine.Spec) { s.Files = fs }
	}
	many := func(n int) []machine.PlacedFile {
		out := make([]machine.PlacedFile, n)
		for i := range out {
			out[i] = file(fmt.Sprintf("/data/f%d", i))
		}
		return out
	}
	tests := []struct {
		name  string
		edit  func(*machine.Spec)
		field string
	}{
		{"empty env name", func(s *machine.Spec) { s.Env = []machine.EnvVar{{Value: marker}} }, "env"},
		{"env name with =", func(s *machine.Spec) { s.Env = []machine.EnvVar{{Name: "A=B", Value: marker}} }, "env"},
		{"env name given twice", func(s *machine.Spec) {
			s.Env = []machine.EnvVar{{Name: "A", Value: marker}, {Name: "A", Value: marker}}
		}, "env"},
		{"relative path", files(file("etc/x")), "files"},
		{"unclean path", files(file("/etc/../x")), "files"},
		{"root path", files(file("/")), "files"},
		{"under /proc", files(file("/proc/x")), "files"},
		{"under /sys", files(file("/sys/x")), "files"},
		{"under /dev", files(file("/dev/x")), "files"},
		{"under /run/mvmd", files(file("/run/mvmd/x")), "files"},
		{"reserved directory itself", files(file("/run/mvmd")), "files"},
		{"mode above 07777", files(machine.PlacedFile{Path: "/etc/x", Content: []byte(marker), Mode: 0o10000}), "files"},
		{"path given twice", files(file("/etc/x"), file("/etc/x")), "files"},
		{"65 files", files(many(65)...), "files"},
		{"over 1 MiB", files(machine.PlacedFile{Path: "/etc/x", Content: []byte(strings.Repeat("a", 1<<20+1)), Mode: 0o600}), "files"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			spec := baseSpec()
			tt.edit(&spec)
			err := e.svc.Create(context.Background(), "m", "r", spec)
			requireKind(t, err, machine.ErrInvalid)
			assert.Contains(t, err.Error(), tt.field, "it names the field")
			assert.NotContains(t, err.Error(), marker, "no value or content in an error")
			e.requireNoMachine(t, "m")

			// Control: the same request without the fault is accepted.
			require.NoError(t, e.svc.Create(context.Background(), "m", "r", baseSpec()))
		})
	}

	t.Run("a refusal of a path names the path", func(t *testing.T) {
		e := newEnv()
		spec := baseSpec()
		spec.Files = []machine.PlacedFile{file("/proc/the-path")}
		err := e.svc.Create(context.Background(), "m", "r", spec)
		requireKind(t, err, machine.ErrInvalid)
		assert.Contains(t, err.Error(), "/proc/the-path")
	})

	t.Run("the limits themselves are accepted", func(t *testing.T) {
		e := newEnv()
		spec := baseSpec()
		spec.Files = many(64)
		require.Len(t, spec.Files, 64, "the setup")
		require.NoError(t, e.svc.Create(context.Background(), "a", "ra", spec))
		spec.Files = []machine.PlacedFile{{Path: "/etc/x", Content: []byte(strings.Repeat("a", 1<<20)), Mode: 0o7777}}
		require.NoError(t, e.svc.Create(context.Background(), "b", "rb", spec))
	})
}

// PlacedFile.Info is what a Machine reports of a file: everything but the content.
func TestPlacedFile_InfoHoldsEverythingButTheContent(t *testing.T) {
	f := machine.PlacedFile{Path: "/etc/x", Content: []byte(marker), Mode: 0o640, UID: 3, GID: 4}
	assert.Equal(t, machine.PlacedFileInfo{Path: "/etc/x", Mode: 0o640, UID: 3, GID: 4, SizeBytes: uint64(len(marker))}, f.Info())
	// Control: the file itself holds the content.
	assert.Equal(t, []byte(marker), f.Content)
}
