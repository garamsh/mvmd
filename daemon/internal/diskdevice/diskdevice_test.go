package diskdevice_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestName_TheImageIsNamedByItsPosition(t *testing.T) {
	assert.Equal(t, "disk0", diskdevice.Name(0, ""))
	assert.Equal(t, "disk3", diskdevice.Name(3, ""))
}

func TestName_ADiskIsNamedByTheHexOfItsIDsSHA256AtAnyPosition(t *testing.T) {
	sum := sha256.Sum256([]byte("data"))
	want := "disk_" + hex.EncodeToString(sum[:])

	assert.Equal(t, want, diskdevice.Name(0, "data"))
	assert.Equal(t, want, diskdevice.Name(5, "data"), "the position does not name a disk")
	assert.NotEqual(t, want, diskdevice.Name(0, "other"), "control: another ID names another device")
}

func TestName_IsWithinAFileNameAndNeverStartsWithTheReservedPrefix(t *testing.T) {
	long := machine.DiskID(strings.Repeat("x", 4096))
	ids := []machine.DiskID{"", "a", "__reserved", long, "with/slash and space"}
	for _, id := range ids {
		name := diskdevice.Name(0, id)
		assert.LessOrEqual(t, len(name), 255)
		assert.False(t, strings.HasPrefix(name, "__"), name)
	}
	assert.Equal(t, len(diskdevice.Name(0, "a")), len(diskdevice.Name(0, long)), "the length does not follow the ID's")
}

func TestRemoveLink_RemovesALinkThatIsPresentAndAcceptsOneThatIsNot(t *testing.T) {
	link := filepath.Join(t.TempDir(), "disk_x")
	require.NoError(t, os.WriteFile(link, []byte("disk"), 0o600))

	require.NoError(t, diskdevice.RemoveLink(link))
	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist, "control: the link that was present is gone")

	assert.NoError(t, diskdevice.RemoveLink(link), "an absent link succeeds")
}

type dirs struct{ dir string }

func (d dirs) RunDir(machine.MachineID) (string, error) { return d.dir, nil }

type failingDirs struct{}

func (failingDirs) RunDir(machine.MachineID) (string, error) {
	return "", errors.New("no such Machine")
}

// diskFile is a regular file for a disk, outside the run directory.
func diskFile(t *testing.T, root string) string {
	t.Helper()
	path := filepath.Join(root, "data.img")
	require.NoError(t, os.WriteFile(path, []byte("a disk"), 0o600))
	return path
}

func TestLink_HardLinksTheFileUnderTheDevicesNameAndReturnsTheRunDirectory(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	file := diskFile(t, root)

	got, err := diskdevice.Link(dirs{run}, "m1", machine.BlockDevice{Disk: "data", Path: file})
	require.NoError(t, err)
	assert.Equal(t, run, got)

	linked, err := os.Lstat(filepath.Join(run, diskdevice.Name(0, "data")))
	require.NoError(t, err)
	orig, err := os.Stat(file)
	require.NoError(t, err)
	assert.True(t, os.SameFile(orig, linked), "the link is the disk's file")
}

func TestLink_ReplacesWhatStandsAtTheLinkWithoutFollowingIt(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	file := diskFile(t, root)
	outside := filepath.Join(root, "outside")
	require.NoError(t, os.WriteFile(outside, []byte("untouched"), 0o600))
	link := filepath.Join(run, diskdevice.Name(0, "data"))
	require.NoError(t, os.Symlink(outside, link))

	_, err := diskdevice.Link(dirs{run}, "m1", machine.BlockDevice{Disk: "data", Path: file})
	require.NoError(t, err)

	linked, err := os.Lstat(link)
	require.NoError(t, err)
	orig, err := os.Stat(file)
	require.NoError(t, err)
	assert.True(t, os.SameFile(orig, linked), "the link is now the disk's file")
	raw, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(raw), "the symbolic link was not followed")
}

func TestLink_RefusesADiskThatCannotBeLinkedAndMakesNoLink(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	good := diskFile(t, root)
	symlink := filepath.Join(root, "symlink.img")
	require.NoError(t, os.Symlink(good, symlink))

	tests := []struct {
		name string
		dev  machine.BlockDevice
		want string
	}{
		{"no disk", machine.BlockDevice{Path: good}, "names no disk"},
		{"a relative path", machine.BlockDevice{Disk: "data", Path: "data.img"}, "absolute"},
		{"a missing file", machine.BlockDevice{Disk: "data", Path: filepath.Join(root, "absent.img")}, "no such file"},
		{"a symbolic link", machine.BlockDevice{Disk: "data", Path: symlink}, "not a regular file"},
		{"a directory", machine.BlockDevice{Disk: "data", Path: root}, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := diskdevice.Link(dirs{run}, "m1", tt.dev)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			_, statErr := os.Lstat(filepath.Join(run, diskdevice.Name(0, "data")))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "no link was made")
		})
	}

	_, err := diskdevice.Link(dirs{run}, "m1", machine.BlockDevice{Disk: "data", Path: good})
	require.NoError(t, err, "control: the same disk as a regular file is linked")
	_, statErr := os.Lstat(filepath.Join(run, diskdevice.Name(0, "data")))
	assert.NoError(t, statErr)
}

func TestLink_ReturnsTheRunDirectoryLookupsFailure(t *testing.T) {
	file := diskFile(t, t.TempDir())
	_, err := diskdevice.Link(failingDirs{}, "m1", machine.BlockDevice{Disk: "data", Path: file})
	assert.ErrorContains(t, err, "no such Machine")
}

func TestLink_NamesTheLinkWithinAFileNameForAnyDiskID(t *testing.T) {
	root := t.TempDir()
	run := filepath.Join(root, "run")
	require.NoError(t, os.Mkdir(run, 0o700))
	file := diskFile(t, root)

	for _, id := range []machine.DiskID{"data", machine.DiskID(strings.Repeat("d", 200))} {
		_, err := diskdevice.Link(dirs{run}, "m1", machine.BlockDevice{Disk: id, Path: file})
		require.NoError(t, err)
		_, err = os.Lstat(filepath.Join(run, diskdevice.Name(0, id)))
		assert.NoError(t, err)
	}
}

func TestRunFiles_NamesEachRecordedDiskByItsDevicesNameAtItsHostPath(t *testing.T) {
	got := diskdevice.RunFiles([]machine.BlockDevice{
		{Disk: "data", Path: "/disks/data.img"},
		{Disk: "data2", Path: "/disks/data2.img", ReadOnly: true},
	})
	assert.Equal(t, []machine.RunFile{
		{Name: diskdevice.Name(0, "data"), Path: "/disks/data.img"},
		{Name: diskdevice.Name(0, "data2"), Path: "/disks/data2.img"},
	}, got)
	assert.NotEqual(t, got[0].Name, got[1].Name)
	assert.Empty(t, diskdevice.RunFiles(nil), "control: no recorded disk lists none")
}
