package supervision_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// hostFile writes a file the VMM's user can read, on the file system the
// state directories are on, and returns its path.
func hostFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(shortDir(t), name)
	require.NoError(t, os.WriteFile(path, []byte("disk"), 0o644))
	require.FileExists(t, path)
	return path
}

// A file the description lists is in the run directory, as a link to the host
// file, when the VMM starts: the probe, as the Machine's user, opens it at its
// path in the root. The control is a name no entry lists.
func TestStart_LinksTheRunFilesBeforeTheVMMStarts(t *testing.T) {
	n := newNode(t)
	src := hostFile(t, "disk")
	run := runDir(n.dir, "m")
	linked, unlisted := machine.InRunDir+"/linked", machine.InRunDir+"/unlisted"

	got := n.started(t, "m", vmmUser, []string{"open:" + linked, "open:" + unlisted}, func(p *machine.ProcessDescription) {
		p.RunFiles = []machine.RunFile{{Name: "linked", Path: src}}
	})
	report := reportOf(t, run)
	assert.Equal(t, run, got)
	assert.Equal(t, "ok", report["open:"+linked], "the VMM opens the link when it starts")
	assert.NotEqual(t, "ok", report["open:"+unlisted], "control: a name no entry lists is not there")

	var host, link syscall.Stat_t
	require.NoError(t, syscall.Stat(src, &host))
	require.NoError(t, syscall.Stat(filepath.Join(run, "linked"), &link))
	assert.Equal(t, host.Ino, link.Ino, "a hard link, not a copy: both names are one file")
}

// An entry whose name is not one file name, or whose path is not absolute, is
// refused before anything is created, so nothing of the list is linked; the
// control is a valid entry, which links.
func TestStart_RefusesARunFileThatIsNotALinkOfOneName(t *testing.T) {
	src := hostFile(t, "disk")
	valid := machine.RunFile{Name: "ok", Path: src}
	for name, bad := range map[string]machine.RunFile{
		"empty name":   {Name: "", Path: src},
		"dot":          {Name: ".", Path: src},
		"dot dot":      {Name: "..", Path: src},
		"slash":        {Name: "a/b", Path: src},
		"leading dots": {Name: "../escape", Path: src},
		"relative":     {Name: "rel", Path: "disk"},
		"empty path":   {Name: "nopath", Path: ""},
	} {
		n := newNode(t)
		proc := vmmProcess(runDir(n.dir, "m"))
		proc.RunFiles = []machine.RunFile{valid, bad}

		err := n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
		require.Error(t, err, name)
		assert.NoDirExists(t, filepath.Join(n.dir, "m"), "%s: nothing was created, so the valid entry before it is not linked", name)
		assert.Empty(t, processes("--machine m "), "%s: no supervisor was started", name)
	}

	// Control: the valid entry alone starts and is linked.
	n := newNode(t)
	run := runDir(n.dir, "m")
	proc := vmmProcess(run)
	proc.RunFiles = []machine.RunFile{valid}
	require.NoError(t, n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour))))
	assert.FileExists(t, filepath.Join(run, "ok"))
}

// A link that cannot be made fails Start with the entry's name in the reason,
// and the VMM is not started; the control is the entry before it, which was
// linked.
func TestStart_AFailedRunFileLinkFailsTheStart(t *testing.T) {
	n := newNode(t)
	src := hostFile(t, "disk")
	run := runDir(n.dir, "m")
	missing := filepath.Join(shortDir(t), "absent")
	proc := describe(probeBin, "report", run)
	proc.RunFiles = []machine.RunFile{{Name: "first", Path: src}, {Name: "second", Path: missing}}

	err := n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "second")
	assert.FileExists(t, filepath.Join(run, "first"), "control: the entry before the failure was linked")
	assert.NoFileExists(t, filepath.Join(run, "second"))
	assert.NoFileExists(t, filepath.Join(run, "report.json"), "the VMM did not start")
	assert.Empty(t, processes("--machine m "), "no supervisor is left")
}
