package supervision_test

import (
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
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

// A run file that is absent, a symbolic link (also one to a regular file) or a
// directory is refused before anything is created; the control is a regular
// file, which starts.
func TestStart_RefusesARunFileThatIsNotARegularFile(t *testing.T) {
	src := hostFile(t, "disk")
	link := filepath.Join(shortDir(t), "link")
	require.NoError(t, os.Symlink(src, link))
	for name, path := range map[string]string{
		"absent":                            filepath.Join(shortDir(t), "absent"),
		"a symbolic link to a regular file": link,
		"a directory":                       shortDir(t),
	} {
		n := newNode(t)
		proc := vmmProcess(runDir(n.dir, "m"))
		proc.RunFiles = []machine.RunFile{{Name: "linked", Path: path}}

		err := n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
		require.Error(t, err, name)
		assert.NoDirExists(t, filepath.Join(n.dir, "m"), "%s: nothing was created", name)
		assert.Empty(t, processes("--machine m "), "%s: no supervisor was started", name)
	}

	// Control: the regular file starts and is linked.
	n := newNode(t)
	run := runDir(n.dir, "m")
	proc := vmmProcess(run)
	proc.RunFiles = []machine.RunFile{{Name: "linked", Path: src}}
	require.NoError(t, n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour))))
	assert.FileExists(t, filepath.Join(run, "linked"))
}

// A leftover at a run file's name, as an earlier run of the Machine leaves, is
// replaced by the link to the source; the control is the leftover's own inode,
// read before the Start, which is not the source's.
func TestStart_ALeftoverAtARunFileNameIsReplacedByTheLink(t *testing.T) {
	n := newNode(t)
	src := hostFile(t, "disk")
	run := runDir(n.dir, "m")
	require.NoError(t, os.MkdirAll(run, 0o700))
	leftover := filepath.Join(run, "linked")
	require.NoError(t, os.WriteFile(leftover, []byte("old"), 0o600))
	var before, host, after syscall.Stat_t
	require.NoError(t, syscall.Stat(leftover, &before))
	require.NoError(t, syscall.Stat(src, &host))
	require.NotEqual(t, host.Ino, before.Ino, "setup: the leftover is another file")

	n.started(t, "m", vmmUser, nil, func(p *machine.ProcessDescription) {
		p.RunFiles = []machine.RunFile{{Name: "linked", Path: src}}
	})
	require.NoError(t, syscall.Stat(leftover, &after))
	assert.Equal(t, host.Ino, after.Ino, "the name is now a link to the source")
}

// A link that cannot be made fails Start with the entry's name in the reason,
// and the VMM is not started; the control is the entry before it, which was
// linked.
func TestStart_AFailedRunFileLinkFailsTheStart(t *testing.T) {
	n := newNode(t)
	src := hostFile(t, "disk")
	run := runDir(n.dir, "m")
	// A non-empty directory at the second name cannot be cleared, so the link
	// fails after the first entry was linked.
	require.NoError(t, os.MkdirAll(filepath.Join(run, "second", "inner"), 0o700))
	proc := describe(probeBin, "report", run)
	proc.RunFiles = []machine.RunFile{{Name: "first", Path: src}, {Name: "second", Path: src}}

	err := n.a.Start(ctx, "m", proc, vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "second")
	assert.FileExists(t, filepath.Join(run, "first"), "control: the entry before the failure was linked")
	assert.NoFileExists(t, filepath.Join(run, "report.json"), "the VMM did not start")
	assert.Empty(t, processes("--machine m "), "no supervisor is left")
}

// The API and vsock sockets an earlier VMM left in the run directory are gone
// when the next VMM starts, since Firecracker fails to bind a path that
// exists. The control is another file of the run directory, which stays.
func TestStart_TheSocketsAnEarlierRunLeftAreRemoved(t *testing.T) {
	n := newNode(t)
	run := runDir(n.dir, "m")
	require.NoError(t, os.MkdirAll(run, 0o700))
	socks := []string{filepath.Join(run, vmmconfig.APISocket), filepath.Join(run, vmmconfig.VsockSocket)}
	for _, sock := range socks {
		l, err := net.Listen("unix", sock)
		require.NoError(t, err)
		l.(*net.UnixListener).SetUnlinkOnClose(false)
		require.NoError(t, l.Close())
		require.FileExists(t, sock, "setup: the closed listener left its socket file")
	}
	other := filepath.Join(run, "other")
	require.NoError(t, os.WriteFile(other, []byte("kept"), 0o600))

	n.started(t, "m", vmmUser, nil, nil)

	for _, sock := range socks {
		assert.NoFileExists(t, sock, "the earlier run's socket is removed")
	}
	kept, err := os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, "kept", string(kept), "control: another file of the run directory is untouched")
}
