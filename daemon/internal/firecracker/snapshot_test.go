package firecracker_test

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// snapshotFiles are the two files Snapshot returns, as the Snapshot Store hands
// them back: somewhere else, under the same names.
func snapshotFiles() []string {
	return []string{"/store/s1/state/snapshot.state", "/store/s1/state/snapshot.mem"}
}

// restoreSpec is a restored Machine's boot description: its image, its own root
// disk at a new path, and the tap it was given.
func (n *node) restoreSpec(tap string) machine.BootSpec {
	spec := n.spec()
	spec.Devices[1].Path = "/disks/restored-root.img"
	spec.Network.Tap = tap
	return spec
}

func TestSnapshot_WritesTheSnapshotThroughTheAPIIntoTheRunDirectory(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	taken, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err)

	seen := f.seen()[before:]
	require.Len(t, seen, 1, "one request, and no other call the lifecycle did not ask for")
	assert.Equal(t, "PUT", seen[0].Method)
	assert.Equal(t, "/snapshot/create", seen[0].Path)
	assert.JSONEq(t, `{"snapshot_type":"Full","snapshot_path":"/run/snapshot.state","mem_file_path":"/run/snapshot.mem"}`, seen[0].Body,
		"the VMM is given paths in its root, and no host path")
	assert.Equal(t, []string{filepath.Join(n.run, "snapshot.state"), filepath.Join(n.run, "snapshot.mem")}, taken.Files,
		"the files are the run directory's, at its host path")
	assert.Equal(t, sum([]byte("a kernel")), taken.KernelDigest, "the digest is the pinned kernel's")
}

func TestSnapshot_NamesItsFilesTheSameOnEverySnapshotOfAMachine(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)

	first, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err)
	second, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err)

	assert.Equal(t, first.Files, second.Files, "two snapshots of one Machine overwrite one pair of files")
	require.Len(t, first.Files, 2)
	assert.NotEqual(t, first.Files[0], first.Files[1], "control: the two files are distinct, so equal slices are not one name twice")
	seen := f.seen()
	assert.Equal(t, seen[len(seen)-2].Body, seen[len(seen)-1].Body, "the VMM is asked for the same paths both times")
}

func TestSnapshot_ReturnsTheVMMsRefusalAndNoFiles(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)

	taken, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err, "control: the same call succeeds when the VMM accepts it")
	assert.NotEmpty(t, taken.Files)

	f.mu.Lock()
	f.answer = func(r request) (int, string) {
		if r.Path == "/snapshot/create" {
			return http.StatusBadRequest, `{"fault_message":"Cannot create a snapshot of a running microVM"}`
		}
		return http.StatusNoContent, ""
	}
	f.mu.Unlock()
	taken, err = n.driver.Snapshot(context.Background(), "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Cannot create a snapshot of a running microVM")
	assert.Empty(t, taken.Files, "a refused snapshot names no file")
}

func TestDescribeRestore_MountsTheSnapshotAndTheMachinesOwnFilesAtFixedPaths(t *testing.T) {
	n := newNode(t)
	spec := n.restoreSpec("tap-m2")
	p, err := n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err)

	assert.Equal(t, []machine.File{
		{Host: "/store/s1/state/snapshot.state", In: "/snapshot/state", ReadOnly: true},
		{Host: "/store/s1/state/snapshot.mem", In: "/snapshot/memory", ReadOnly: true},
		{Host: "/images/erofs", In: machine.InDisk(0), ReadOnly: true},
		{Host: "/disks/restored-root.img", In: machine.InDisk(1)},
	}, p.BootFiles, "the Machine's own files are bound where the snapshot recorded them (ADR 0019); no kernel and no initrd")

	booted, err := n.driver.Describe(context.Background(), "m2", spec)
	require.NoError(t, err)
	assert.Equal(t, booted.Executable, p.Executable)
	assert.Equal(t, booted.ExecutableSHA256, p.ExecutableSHA256, "the VMM binary is the pinned one, as it is for a boot")
	assert.Equal(t, booted.Args, p.Args)
	assert.Equal(t, booted.DeviceNodes, p.DeviceNodes, "a Machine with a tap needs /dev/net/tun")
	assert.Equal(t, booted.Overhead, p.Overhead)
	assert.Contains(t, p.DeviceNodes, "/dev/net/tun")
}

func TestDescribeRestore_RefusesWhatIsNotTheSnapshotsOwnPairOfFiles(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err, "control: the pair a Snapshot returns is accepted")

	tests := []struct {
		name  string
		files []string
	}{
		{"no file", nil},
		{"the state file alone", snapshotFiles()[:1]},
		{"the memory file alone", snapshotFiles()[1:]},
		{"two state files", []string{"/a/snapshot.state", "/b/snapshot.state"}},
		{"a third file", append(snapshotFiles(), "/store/s1/state/other")},
		{"files under other names", []string{"/store/state", "/store/mem"}},
		{"a relative path", []string{"snapshot.state", "/store/snapshot.mem"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), tt.files)
			require.Error(t, err)
		})
	}
}

func TestDescribeRestore_RefusesABootDescriptionDescribeRefuses(t *testing.T) {
	n := newNode(t)
	spec := n.restoreSpec("tap-m2")
	_, err := n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err, "control: the description is otherwise accepted")

	spec.Network.Tap = "a-tap-name-over-fifteen"
	_, err = n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	assert.Error(t, err)
}

func TestRestore_LoadsTheSnapshotOntoTheNewMachinesTapAndResumesTheGuest(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	_, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)

	require.NoError(t, n.driver.Restore(ctx, "m2"))

	seen := f.seen()
	require.Len(t, seen, 2)
	assert.Equal(t, "GET", seen[0].Method)
	assert.Equal(t, "/version", seen[0].Path)
	assert.Equal(t, "PUT", seen[1].Method)
	assert.Equal(t, "/snapshot/load", seen[1].Path)
	// The interface is the one Boot named, now on the new Machine's tap; the
	// MAC is left to the snapshot, which records it, and no interface is
	// configured beside the load, which Firecracker refuses after one.
	assert.JSONEq(t, `{
		"snapshot_path": "/snapshot/state",
		"mem_backend": {"backend_type": "File", "backend_path": "/snapshot/memory"},
		"resume_vm": true,
		"network_overrides": [{"iface_id": "eth0", "host_dev_name": "tap-m2"}]
	}`, seen[1].Body)
}

func TestRestore_TakesTheTapOfTheMachineItRestoresAndNoOtherMachinesTap(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	_, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)
	_, err = n.driver.DescribeRestore(ctx, "m3", n.restoreSpec("tap-m3"), snapshotFiles())
	require.NoError(t, err)

	require.NoError(t, n.driver.Restore(ctx, "m2"))
	seen := f.seen()
	assert.Contains(t, seen[len(seen)-1].Body, `"host_dev_name":"tap-m2"`)

	require.NoError(t, n.driver.Restore(ctx, "m3"))
	seen = f.seen()
	assert.Contains(t, seen[len(seen)-1].Body, `"host_dev_name":"tap-m3"`, "the second description was not lost to the first Restore")
}

func TestRestore_AMachineWithNoTapOverridesNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	spec := n.restoreSpec("")
	_, err := n.driver.DescribeRestore(ctx, "m2", spec, snapshotFiles())
	require.NoError(t, err)

	require.NoError(t, n.driver.Restore(ctx, "m2"))
	seen := f.seen()
	assert.JSONEq(t, `{"snapshot_path":"/snapshot/state","mem_backend":{"backend_type":"File","backend_path":"/snapshot/memory"},"resume_vm":true}`, seen[len(seen)-1].Body)
}

func TestRestore_NamesOnlyPathsTheDescriptionMounts(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	p, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)
	require.NoError(t, n.driver.Restore(ctx, "m2"))

	mounted := map[string]bool{}
	for _, bf := range p.BootFiles {
		mounted[bf.In] = true
	}
	for _, in := range []string{"/snapshot/state", "/snapshot/memory"} {
		assert.True(t, mounted[in], "%s is in the VMM's request and mounted", in)
	}
	body := f.seen()[1].Body
	assert.Contains(t, body, `"snapshot_path":"/snapshot/state"`)
	assert.Contains(t, body, `"backend_path":"/snapshot/memory"`)
	for _, bf := range p.BootFiles {
		assert.NotContains(t, body, bf.Host, "the request names no host path")
	}
}

func TestRestore_RefusesAMachineWhoseRestoreWasNotDescribed(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	ctx := context.Background()

	require.Error(t, n.driver.Restore(ctx, "m2"))
	assert.Empty(t, f.seen(), "nothing reaches the VMM for a Machine no description named")

	_, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)
	require.NoError(t, n.driver.Restore(ctx, "m2"), "control: the same call is accepted once described")
	assert.NotEmpty(t, f.seen())
}

func TestRestore_SpendsTheDescriptionWhetherTheLoadSucceedsOrNot(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/snapshot/load" {
			return http.StatusBadRequest, `{"fault_message":"Error manipulating the backing file: No such file or directory"}`
		}
		if r.Path == "/version" {
			return http.StatusOK, `{"firecracker_version":"1.17.0"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	_, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)

	err = n.driver.Restore(ctx, "m2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Error manipulating the backing file")
	loads := len(f.seen())

	err = n.driver.Restore(ctx, "m2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was described", "a repeat is refused for want of a description, not sent again")
	assert.Len(t, f.seen(), loads, "nothing more reached the VMM")
}

func TestRestore_RefusesAVMMOfAnotherVersionAndLoadsNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		if r.Path == "/version" {
			return http.StatusOK, `{"firecracker_version":"1.16.0"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())
	ctx := context.Background()
	_, err := n.driver.DescribeRestore(ctx, "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)

	err = n.driver.Restore(ctx, "m2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "1.16.0")
	for _, r := range f.seen() {
		assert.NotEqual(t, "/snapshot/load", r.Path, "no snapshot is loaded into another release")
	}
}
