package cloudhypervisor_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// dataDrive is the name of the device of the disk "data": disk_ and the hex of
// the SHA-256 of the ID.
const dataDrive = "disk_3a6eb0790f39ac87c94f3856b2dd2c5d110e6811602261a9a923d3bb23adc8b7"

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

func TestAttachDisk_AddsTheDiskAtAHardLinkInTheRunDirectory(t *testing.T) {
	tests := []struct {
		name     string
		readOnly bool
		body     string
	}{
		{"a read-write disk", false, `{"id":"` + dataDrive + `","path":"/run/` + dataDrive + `","readonly":false,"image_type":"Raw"}`},
		{"a read-only disk", true, `{"id":"` + dataDrive + `","path":"/run/` + dataDrive + `","readonly":true,"image_type":"Raw"}`},
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
			require.Len(t, got, 1, "one request")
			assert.Equal(t, "PUT", got[0].Method)
			assert.Equal(t, "/vm.add-disk", got[0].Path)
			assert.JSONEq(t, tt.body, got[0].Body)

			linked, err := os.Stat(filepath.Join(n.run, dataDrive))
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
	link := filepath.Join(n.run, dataDrive)
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
			_, statErr := os.Lstat(filepath.Join(n.run, dataDrive))
			assert.ErrorIs(t, statErr, os.ErrNotExist, "no link was made")
		})
	}

	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", good), "control: the same disk as a regular file is attached")
	assert.Len(t, since(f, before), 1)
}

// A disk ID has no length rule, and the device's name is the link's file name.
func TestAttachDisk_NamesTheLinkWithinAFileNameForAnyDiskID(t *testing.T) {
	for _, tt := range []struct{ name, id string }{
		{"control: a short ID", "data"},
		{"an ID the hex of which exceeds a file name's limit", strings.Repeat("d", 200)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{}
			n.booted(f)
			before := len(f.seen())
			path := filepath.Join(filepath.Dir(n.run), "long.img")
			require.NoError(t, os.WriteFile(path, []byte("a disk"), 0o600))

			require.NoError(t, n.driver.AttachDisk(context.Background(), "m1",
				machine.BlockDevice{Disk: machine.DiskID(tt.id), Path: path}))

			got := since(f, before)
			require.Len(t, got, 1)
			name := strings.Split(strings.Split(got[0].Body, `"path":"/run/`)[1], `"`)[0]
			assert.LessOrEqual(t, len(name), 255)
			assert.Regexp(t, `^disk_[0-9a-f]+$`, name, "no __ prefix, which the VMM reserves")
			_, err := os.Lstat(filepath.Join(n.run, name))
			assert.NoError(t, err)
		})
	}
}

func TestAttachDisk_ARefusalByTheVMMLeavesNoLink(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	dev := n.disk("data", false)
	link := filepath.Join(n.run, dataDrive)

	f.answer = func(r request) (int, string) {
		if r.Path == "/vm.add-disk" {
			return http.StatusInternalServerError, `no PCI segment is free`
		}
		return http.StatusNoContent, ""
	}
	err := n.driver.AttachDisk(context.Background(), "m1", dev)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no PCI segment is free", "the VMM's reason is returned")
	assert.NotErrorIs(t, err, machine.ErrUnsupported)
	_, statErr := os.Lstat(link)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the failed attach left no link")

	f.answer = nil
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev), "control: with the VMM's consent the link stays")
	_, statErr = os.Lstat(link)
	assert.NoError(t, statErr)
}

func TestDetachDisk_RemovesTheDeviceAndItsLink(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	dev := n.disk("data", false)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", dev))
	before := len(f.seen())

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"))

	got := since(f, before)
	require.NotEmpty(t, got)
	assert.Equal(t, request{Method: "PUT", Path: "/vm.remove-device", Body: `{"id":"` + dataDrive + `"}`}, got[0])
	_, err := os.Lstat(filepath.Join(n.run, dataDrive))
	assert.ErrorIs(t, err, os.ErrNotExist, "no link holds the file")
	_, err = os.Stat(dev.Path)
	assert.NoError(t, err, "the disk's own file is untouched")
}

// The VMM keeps a device's identifier until the guest has ejected it, and
// refuses an add of the same one before then, so a detach returns after the
// device has left the device tree and not before.
func TestDetachDisk_WaitsForTheGuestToEjectTheDevice(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", n.disk("data", false)))
	link := filepath.Join(n.run, dataDrive)

	var polls atomic.Int32
	f.answer = func(r request) (int, string) {
		if r.Method == "GET" && r.Path == "/vm.info" {
			if polls.Add(1) < 4 {
				if _, err := os.Lstat(link); err != nil {
					t.Error("the link was removed while the VMM still held the device")
				}
				return http.StatusOK, `{"config":{},"state":"Running","device_tree":{"` + dataDrive + `":{"id":"` + dataDrive + `"}}}`
			}
			return http.StatusOK, `{"config":{},"state":"Running","device_tree":{"_disk0":{}}}`
		}
		return http.StatusNoContent, ""
	}

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"))

	assert.Equal(t, int32(4), polls.Load(), "asked again until the device was gone")
	_, err := os.Lstat(link)
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestDetachDisk_ReportsAGuestThatDoesNotEjectAndStillReleasesTheLink(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", n.disk("data", false)))
	f.answer = func(r request) (int, string) {
		if r.Method == "GET" && r.Path == "/vm.info" {
			return http.StatusOK, `{"config":{},"state":"Running","device_tree":{"` + dataDrive + `":{}}}`
		}
		return http.StatusNoContent, ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := n.driver.DetachDisk(ctx, "m1", "data")

	require.Error(t, err)
	assert.Contains(t, err.Error(), "did not eject")
	_, statErr := os.Lstat(filepath.Join(n.run, dataDrive))
	assert.ErrorIs(t, statErr, os.ErrNotExist, "the VMM no longer holds the file, so the link goes")
}

func TestDetachDisk_KeepsTheLinkWhenTheVMMRefuses(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	require.NoError(t, n.driver.AttachDisk(context.Background(), "m1", n.disk("data", false)))
	link := filepath.Join(n.run, dataDrive)

	f.answer = func(r request) (int, string) {
		if r.Path == "/vm.remove-device" {
			return http.StatusInternalServerError, `no such device`
		}
		return http.StatusNoContent, ""
	}
	err := n.driver.DetachDisk(context.Background(), "m1", "data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no such device")
	_, statErr := os.Lstat(link)
	assert.NoError(t, statErr, "the VMM may still have the device, so its link stays")

	f.answer = nil
	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"), "control: once the VMM agrees the link goes")
	_, statErr = os.Lstat(link)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}

func TestDetachDisk_RefusesNoDisk(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	require.Error(t, n.driver.DetachDisk(context.Background(), "m1", ""))
	assert.Len(t, f.seen(), before, "the VMM was asked nothing")

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"), "control: a disk is detached")
	assert.Greater(t, len(f.seen()), before)
}

// A disk given at boot is detached as one attached later is: the device a boot
// names for it carries the name a detach derives, and it has no link.
func TestDetachDisk_FindsADiskGivenAtBoot(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	spec := n.spec()
	spec.Devices = append(spec.Devices, machine.BlockDevice{Disk: "data", Path: "/disks/data.img"})
	p, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	assert.Contains(t, p.Args, "path=/disk/2,readonly=off,image_type=raw,id="+dataDrive, "control: boot named the device of the disk it was given")
	require.NoError(t, n.driver.Boot(context.Background(), "m1", spec))
	before := len(f.seen())

	require.NoError(t, n.driver.DetachDisk(context.Background(), "m1", "data"))

	got := since(f, before)
	require.NotEmpty(t, got)
	assert.Equal(t, request{Method: "PUT", Path: "/vm.remove-device", Body: `{"id":"` + dataDrive + `"}`}, got[0])
}

// infoWith makes the VMM answer GET /vm.info with the disks given as id and
// path pairs.
func infoWith(f *fakeAPI, disks ...[2]string) {
	body := `{"state":"Running","config":{"disks":[`
	for i, d := range disks {
		if i > 0 {
			body += ","
		}
		body += `{"id":"` + d[0] + `","path":"` + d[1] + `","readonly":false}`
	}
	body += `]}}`
	f.answer = func(r request) (int, string) {
		if r.Method == "GET" && r.Path == "/vm.info" {
			return http.StatusOK, body
		}
		return http.StatusNoContent, ""
	}
}

func TestDrive_ReadsTheVMMsConfigurationAndTellsHowTheDriveCameToBe(t *testing.T) {
	tests := []struct {
		name  string
		disks [][2]string
		want  machine.DriveState
	}{
		{"no device of the disk", [][2]string{{"disk0", "/disk/0"}}, machine.DriveAbsent},
		{"a device given at boot", [][2]string{{"disk0", "/disk/0"}, {dataDrive, "/disk/2"}}, machine.DriveAtBoot},
		{"a device added to the running VMM", [][2]string{{"disk0", "/disk/0"}, {dataDrive, "/run/" + dataDrive}}, machine.DriveHotPlugged},
		{"another disk's device in the run directory", [][2]string{{"disk_other", "/run/disk_other"}}, machine.DriveAbsent},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{}
			n.booted(f)
			infoWith(f, tt.disks...)
			before := len(f.seen())

			got, err := n.driver.Drive(context.Background(), "m1", "data")

			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
			sent := since(f, before)
			require.Len(t, sent, 1, "a read, and nothing that changes the VMM")
			assert.Equal(t, request{Method: "GET", Path: "/vm.info"}, sent[0])
		})
	}
}

func TestDrive_ReportsAVMMThatCannotAnswerAndRefusesNoDisk(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.answer = func(request) (int, string) { return http.StatusInternalServerError, `not now` }

	_, err := n.driver.Drive(context.Background(), "m1", "data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not now")

	infoWith(f, [2]string{dataDrive, "/run/" + dataDrive})
	_, err = n.driver.Drive(context.Background(), "m1", "")
	require.Error(t, err, "no disk is named")
	got, err := n.driver.Drive(context.Background(), "m1", "data")
	require.NoError(t, err, "control: the VMM that answers is read")
	assert.Equal(t, machine.DriveHotPlugged, got)
}
