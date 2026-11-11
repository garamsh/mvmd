package cloudhypervisor_test

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// snapshotFiles are the three files Snapshot returns, as the Snapshot Store
// hands them back: in a directory of their own, in any order.
func snapshotFiles() []string {
	return []string{"/store/s1/state/config.json", "/store/s1/state/state.json", "/store/s1/state/memory-ranges"}
}

// restoreSpec is a restored Machine's boot description: its image, its own root
// disk at a new path, and the tap it was given.
func (n *node) restoreSpec(tap string) machine.BootSpec {
	spec := n.spec()
	spec.Devices[1].Path = "/disks/restored-root.img"
	spec.Network.Tap = tap
	return spec
}

// vmm stands in for the VMM's side of a snapshot: it creates each file and
// refuses one that exists, as v54.0 does (create_new). It records which of the
// files were there when a request came.
type vmm struct {
	mu      sync.Mutex
	run     string
	present [][]string
	fail    bool // leave the files and answer an error, as a snapshot that failed part way
}

func (v *vmm) answer(r request) (int, string) {
	switch {
	case r.Path == "/vmm.ping":
		return http.StatusOK, `{"version":"v54.0"}`
	case r.Path == "/vm.info":
		return http.StatusOK, `{"state":"Running"}`
	case r.Path != "/vm.snapshot":
		return http.StatusNoContent, ""
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	var there []string
	for _, name := range []string{"config.json", "state.json", "memory-ranges"} {
		if _, err := os.Stat(filepath.Join(v.run, name)); err == nil {
			there = append(there, name)
		}
	}
	v.present = append(v.present, there)
	if len(there) > 0 {
		return http.StatusInternalServerError, `{"error":"File exists (os error 17)"}`
	}
	if v.fail {
		_ = os.WriteFile(filepath.Join(v.run, "config.json"), []byte("{}"), 0o600)
		_ = os.WriteFile(filepath.Join(v.run, "state.json"), []byte("{}"), 0o600)
		return http.StatusInternalServerError, `{"error":"no space left on device"}`
	}
	for _, name := range []string{"config.json", "state.json", "memory-ranges"} {
		_ = os.WriteFile(filepath.Join(v.run, name), []byte("{}"), 0o600)
	}
	return http.StatusNoContent, ""
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
	assert.Equal(t, "/vm.snapshot", seen[0].Path)
	assert.JSONEq(t, `{"destination_url":"file:///run"}`, seen[0].Body, "the VMM is given its root's run directory, and no host path")
	assert.Equal(t, []string{filepath.Join(n.run, "config.json"), filepath.Join(n.run, "state.json"), filepath.Join(n.run, "memory-ranges")}, taken.Files,
		"the files are the run directory's, at its host path")
}

// v54.0 creates each file and refuses one that exists, so a second Snapshot of a
// Machine answers only if the first one's files are gone.
func TestSnapshot_NamesItsFilesTheSameOnEverySnapshotOfAMachine(t *testing.T) {
	n := newNode(t)
	v := &vmm{run: n.run}
	f := &fakeAPI{answer: v.answer}
	n.booted(f)

	first, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err)
	second, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err, "the VMM refuses a file that exists, so the first snapshot's files were removed")

	assert.Equal(t, first.Files, second.Files, "two snapshots of one Machine hold one set of files")
	require.Len(t, first.Files, 3)
	assert.Len(t, map[string]bool{first.Files[0]: true, first.Files[1]: true, first.Files[2]: true}, 3,
		"control: the three files are distinct, so equal slices are not one name three times")
	assert.Equal(t, [][]string{nil, nil}, v.present, "no earlier file was there when the VMM was asked, either time")
}

func TestSnapshot_ASnapshotAfterAFailedAttemptThatLeftFilesSucceeds(t *testing.T) {
	n := newNode(t)
	v := &vmm{run: n.run, fail: true}
	f := &fakeAPI{answer: v.answer}
	n.booted(f)

	_, err := n.driver.Snapshot(context.Background(), "m1")
	require.Error(t, err)
	assert.FileExists(t, filepath.Join(n.run, "config.json"), "control: the failed attempt left files")

	v.mu.Lock()
	v.fail = false
	v.mu.Unlock()
	taken, err := n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err)
	assert.Len(t, taken.Files, 3)
	assert.Equal(t, [][]string{nil, nil}, v.present, "the files the failed attempt left were removed before the next request")
}

func TestSnapshot_RefusesWhatItCannotRemoveAndSendsNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())
	// A directory by a snapshot file's name that is not empty: os.Remove refuses.
	blocker := filepath.Join(n.run, "state.json")
	require.NoError(t, os.Mkdir(blocker, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(blocker, "x"), nil, 0o600))

	_, err := n.driver.Snapshot(context.Background(), "m1")
	require.Error(t, err)
	assert.Len(t, f.seen(), before, "no request reached the VMM")

	require.NoError(t, os.RemoveAll(blocker))
	_, err = n.driver.Snapshot(context.Background(), "m1")
	require.NoError(t, err, "control: the same call succeeds once the blocker is gone")
	sent := f.seen()[before:]
	require.Len(t, sent, 1)
	assert.Equal(t, "/vm.snapshot", sent[0].Path)
	assert.Equal(t, "PUT", sent[0].Method)
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
		if r.Path == "/vm.snapshot" {
			return http.StatusInternalServerError, `{"error":"The VM is not paused"}`
		}
		return http.StatusNoContent, ""
	}
	f.mu.Unlock()
	taken, err = n.driver.Snapshot(context.Background(), "m1")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "The VM is not paused")
	assert.Empty(t, taken.Files, "a refused snapshot names no file")
}

func TestDescribeRestore_MountsTheSnapshotTheInitrdAndTheMachinesOwnFilesAtFixedPaths(t *testing.T) {
	n := newNode(t)
	spec := n.restoreSpec("tap-m2")
	p, err := n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err)

	assert.Equal(t, []machine.File{
		{Host: n.cfg.Initrd, In: "/boot/initrd", ReadOnly: true, SHA256: n.cfg.InitrdSHA256},
		{Host: "/store/s1/state/config.json", In: "/snapshot/config.json", ReadOnly: true},
		{Host: "/store/s1/state/state.json", In: "/snapshot/state.json", ReadOnly: true},
		{Host: "/store/s1/state/memory-ranges", In: "/snapshot/memory-ranges", ReadOnly: true},
		{Host: "/images/erofs", In: machine.InDisk(0), ReadOnly: true},
		{Host: "/disks/restored-root.img", In: machine.InDisk(1)},
	}, p.BootFiles, "the initrd the VMM opens before it restores (ADR 0027), the snapshot, then the Machine's own files where the snapshot recorded them (ADR 0019); no kernel")

	booted, err := n.driver.Describe(context.Background(), "m2", spec)
	require.NoError(t, err)
	assert.Equal(t, booted.Executable, p.Executable)
	assert.Equal(t, booted.ExecutableSHA256, p.ExecutableSHA256, "the VMM binary is the pinned one, as it is for a boot")
	assert.Equal(t, booted.DeviceNodes, p.DeviceNodes)
	assert.Equal(t, booted.Overhead, p.Overhead)
	assert.Equal(t, booted.EndOn, p.EndOn, "a guest that resets ends a restored VMM too (ADR 0026)")
	assert.True(t, p.TapByDescriptor, "the new Machine's tap is the VMM's descriptor 3")
}

func TestDescribeRestore_ListsEachRecordedDiskAsARunFileAndBindsNone(t *testing.T) {
	n := newNode(t)
	own, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)
	assert.Empty(t, own.RunFiles, "control: a restore of a Machine with no recorded disk lists none")

	spec := n.restoreSpec("tap-m2")
	spec.RecordedDisks = append(spec.RecordedDisks,
		machine.BlockDevice{Disk: "data", Path: "/disks/data.img"},
		machine.BlockDevice{Disk: "data2", Path: "/disks/data2.img", ReadOnly: true})
	p, err := n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err)

	assert.Equal(t, []machine.RunFile{
		{Name: dataDrive, Path: "/disks/data.img"},
		{Name: diskdevice.Name(0, "data2"), Path: "/disks/data2.img"},
	}, p.RunFiles, "each recorded disk is linked in the run directory under its device's name, at its host path")
	assert.NotEqual(t, p.RunFiles[0].Name, p.RunFiles[1].Name)
	assert.Equal(t, own.BootFiles, p.BootFiles, "the image and the root disk stay bound at /disk/0 and /disk/1, and a recorded disk is not bound at /disk/2")
}

func TestDescribeRestore_RestoresFromTheMountedSnapshotOntoTheTapAndResumes(t *testing.T) {
	n := newNode(t)
	p, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err)

	assert.Equal(t, []string{
		"--api-socket", "path=/run/api.sock",
		"--log-file", "/run/cloud-hypervisor.log",
		"--event-monitor", "fd=7",
		"--restore", "source_url=file:///snapshot,resume=true,net_fds=[net0@[3]]",
	}, p.Args, "the snapshot's own configuration is the VM's: no kernel, no device, and no host path")

	spec := n.restoreSpec("")
	p, err = n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err)
	assert.Equal(t, "source_url=file:///snapshot,resume=true", p.Args[len(p.Args)-1], "no tap, no net_fds")
	assert.False(t, p.TapByDescriptor)
}

func TestDescribeRestore_TakesTheFilesInAnyOrderByName(t *testing.T) {
	n := newNode(t)
	files := snapshotFiles()
	files[0], files[2] = files[2], files[0]
	p, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), files)
	require.NoError(t, err)
	assert.Equal(t, "/snapshot/config.json", p.BootFiles[1].In)
	assert.Equal(t, "/store/s1/state/config.json", p.BootFiles[1].Host)
	assert.Equal(t, "/store/s1/state/memory-ranges", p.BootFiles[3].Host)
}

func TestDescribeRestore_RefusesFilesThatAreNotTheThreeASnapshotHolds(t *testing.T) {
	n := newNode(t)
	_, err := n.driver.DescribeRestore(context.Background(), "m2", n.restoreSpec("tap-m2"), snapshotFiles())
	require.NoError(t, err, "control: the same spec is accepted with the three files")
	tests := []struct {
		name  string
		files []string
	}{
		{"none", nil},
		{"the config alone", snapshotFiles()[:1]},
		{"the memory alone", snapshotFiles()[2:]},
		{"two configs", []string{"/a/config.json", "/b/config.json", "/a/memory-ranges"}},
		{"a fourth file", append(snapshotFiles(), "/store/s1/state/other")},
		{"another name", []string{"/s/config.json", "/s/state.json", "/s/memory"}},
		{"a relative path", []string{"config.json", "/s/state.json", "/s/memory-ranges"}},
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

func TestRestore_WaitsForTheRestoredVMToBeRunningAndSendsNoConfiguration(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	f.listen(t, n.apiSocket())
	spec := n.restoreSpec("tap-m2")
	_, err := n.driver.DescribeRestore(context.Background(), "m2", spec, snapshotFiles())
	require.NoError(t, err)

	require.NoError(t, n.driver.Restore(context.Background(), "m2", spec))

	seen := f.seen()
	require.Len(t, seen, 2, "the process restores itself from its command line")
	assert.Equal(t, request{"GET", "/vmm.ping", ""}, seen[0])
	assert.Equal(t, request{"GET", "/vm.info", ""}, seen[1])
}

func TestRestore_FailsForAVMThatIsNotRunning(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{answer: func(r request) (int, string) {
		switch r.Path {
		case "/vmm.ping":
			return http.StatusOK, `{"version":"v54.0"}`
		case "/vm.info":
			return http.StatusOK, `{"state":"Paused"}`
		}
		return http.StatusNoContent, ""
	}}
	f.listen(t, n.apiSocket())

	err := n.driver.Restore(context.Background(), "m2", n.restoreSpec("tap-m2"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Paused")
}
