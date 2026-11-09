package firecracker_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// disk makes a disk file beside the node's run directory, so that a hard link
// between the two is possible, and returns the device that holds it.
func (n *node) disk(name string, readOnly bool) machine.BlockDevice {
	n.t.Helper()
	path := filepath.Join(filepath.Dir(n.run), name+".img")
	require.NoError(n.t, os.WriteFile(path, []byte("a disk"), 0o600))
	return machine.BlockDevice{Disk: machine.DiskID(name), Path: path, ReadOnly: readOnly}
}

// since returns the requests after the boot's.
func since(f *fakeAPI, before int) []request { return f.seen()[before:] }

func TestAttachDisk_PutsTheDriveAtAHardLinkInTheRunDirectory(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		body     string
	}{
		{"a read-write disk", false, `{"drive_id":"disk_64617461","path_on_host":"/run/disk_64617461","is_root_device":false,"is_read_only":false,"cache_type":"Writeback"}`},
		{"a read-only disk", true, `{"drive_id":"disk_64617461","path_on_host":"/run/disk_64617461","is_root_device":false,"is_read_only":true}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{}
			n.booted(f)
			before := len(f.seen())
			dev := n.disk("data", tt.readOnly)

			require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev))

			got := since(f, before)
			require.Len(t, got, 1, "one request, and no GET /version before it")
			assert.Equal(t, "PUT", got[0].Method)
			assert.Equal(t, "/drives/disk_64617461", got[0].Path)
			assert.JSONEq(t, tt.body, got[0].Body)

			link := filepath.Join(n.run, "disk_64617461")
			linked, err := os.Stat(link)
			require.NoError(t, err)
			orig, err := os.Stat(dev.Path)
			require.NoError(t, err)
			assert.True(t, os.SameFile(orig, linked), "the link is the disk's own file, not a copy")
		})
	}
}

func TestAttachDisk_ALinkIsReplacedAndNothingItNamesIsFollowed(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	dev := n.disk("data", false)

	// What a failed attach left, and what the VMM's user could put there.
	outside := filepath.Join(filepath.Dir(n.run), "outside")
	require.NoError(t, os.WriteFile(outside, []byte("untouched"), 0o600))
	link := filepath.Join(n.run, "disk_64617461")
	require.NoError(t, os.Symlink(outside, link))

	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev))

	linked, err := os.Lstat(link)
	require.NoError(t, err)
	orig, err := os.Stat(dev.Path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(orig, linked), "the link is now the disk's file")
	raw, err := os.ReadFile(outside)
	require.NoError(t, err)
	assert.Equal(t, "untouched", string(raw), "the symbolic link was not followed")
}

func TestAttachDisk_RefusesADiskThatCannotBeLinkedAndSendsNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())
	good := n.disk("data", false)
	target := n.disk("target", false)
	symlink := filepath.Join(filepath.Dir(n.run), "symlink.img")
	require.NoError(t, os.Symlink(target.Path, symlink))

	tests := []struct {
		name string
		dev  machine.BlockDevice
		want string
	}{
		{"no disk", machine.BlockDevice{Path: good.Path}, "names no disk"},
		{"a relative path", machine.BlockDevice{Disk: "data", Path: "data.img"}, "absolute"},
		{"a missing file", machine.BlockDevice{Disk: "data", Path: filepath.Join(filepath.Dir(n.run), "absent.img")}, "no such file"},
		{"a symbolic link", machine.BlockDevice{Disk: "data", Path: symlink}, "not a regular file"},
		{"a directory", machine.BlockDevice{Disk: "data", Path: filepath.Dir(n.run)}, "not a regular file"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := n.driver.AttachDisk(context.Background(), "m1", tt.dev)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Empty(t, since(f, before), "the VMM was asked nothing")
			_, statErr := os.Lstat(filepath.Join(n.run, "disk_64617461"))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "no link was made")
		})
	}

	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", good), "control: the same disk as a regular file is attached")
	assert.Len(t, since(f, before), 1)
}

func TestAttachDisk_ARefusalByTheVMMLeavesNoLink(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	dev := n.disk("data", false)
	link := filepath.Join(n.run, "disk_64617461")

	f.answer = func(r request) (int, string) {
		if r.Path == "/drives/disk_64617461" {
			return http.StatusBadRequest, `{"fault_message":"cannot hot-plug without PCI"}`
		}
		return http.StatusNoContent, ""
	}
	err := n.driver.AttachDisk(context.Background(), "m1", dev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot hot-plug without PCI", "Firecracker's reason is returned")
	assert.NotErrorIs(t, err, machine.ErrUnsupported)
	_, statErr := os.Lstat(link)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the failed attach left no link")

	f.answer = nil
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev), "control: with the VMM's consent the link stays")
	_, statErr = os.Lstat(link)
	assert.NoError(t, statErr)
}

func TestDetachDisk_UnplugsTheDriveAndRemovesItsLink(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	dev := n.disk("data", false)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev))
	before := len(f.seen())

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"))

	got := since(f, before)
	require.Len(t, got, 1)
	assert.Equal(t, "DELETE", got[0].Method)
	assert.Equal(t, "/drives/disk_64617461", got[0].Path)
	_, err := os.Lstat(filepath.Join(n.run, "disk_64617461"))
	assert.ErrorIs(t, err, os.ErrNotExist, "no link holds the file")
	_, err = os.Stat(dev.Path)
	assert.NoError(t, err, "the disk's own file is untouched")
}

func TestDetachDisk_KeepsTheLinkWhenTheVMMRefuses(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", n.disk("data", false)))
	link := filepath.Join(n.run, "disk_64617461")

	f.answer = func(r request) (int, string) {
		if r.Method == "DELETE" {
			return http.StatusBadRequest, `{"fault_message":"the device is busy"}`
		}
		return http.StatusNoContent, ""
	}
	err := n.driver.DetachDisk(context.Background(), "m1", "data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the device is busy")
	_, statErr := os.Lstat(link)
	assert.NoError(t, statErr, "the VMM still has the drive, so its link stays")

	f.answer = nil
	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"), "control: once the VMM agrees the link goes")
	_, statErr = os.Lstat(link)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

// A disk given at boot is detached as one attached later is: the drive a boot
// puts for it carries the name a detach derives, and it has no link.
func TestDetachDisk_FindsADiskGivenAtBoot(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	spec := n.spec()
	spec.Devices = append(spec.Devices, machine.BlockDevice{Disk: "data", Path: "/disks/data.img"})
	_, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	require.NoError(t, n.driver.Boot(context.Background(), "m1", spec))
	var put string
	for _, r := range f.seen() {
		if r.Method == "PUT" && r.Path == "/drives/disk_64617461" {
			put = r.Path
		}
	}
	require.Equal(t, "/drives/disk_64617461", put, "control: boot named the drive of the disk it was given")
	before := len(f.seen())

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"))

	got := since(f, before)
	require.Len(t, got, 1)
	assert.Equal(t, request{Method: "DELETE", Path: put}, got[0])
}

func TestResize_RefusesAndNamesWhyV1170CannotAndSendsNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	err := n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20})
	require.ErrorIs(t, err, machine.ErrUnsupported)
	for _, reason := range []string{"vCPU count before boot only", "/machine-config", "virtio-mem", "docs/memory-hotplug.md", "balloon", "before machine startup"} {
		assert.Contains(t, err.Error(), reason)
	}
	assert.Empty(t, since(f, before), "the VMM was asked nothing")
	assert.NotContains(t, n.driver.Info().Capabilities, machine.CapabilityResize, "Info does not report what is refused")
}
