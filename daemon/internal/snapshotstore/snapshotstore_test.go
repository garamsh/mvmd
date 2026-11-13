package snapshotstore_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/snapshotstore"
)

var ctx = context.Background()

const diskSize = 4 << 20

// fixture is a node's state directory: the snapshots directory under test and
// the run directories whose files a Put takes in.
type fixture struct {
	root string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return fixture{root: t.TempDir()}
}

func (f fixture) snapshots() string { return filepath.Join(f.root, "snapshots") }

func (f fixture) open(t *testing.T) *snapshotstore.Store {
	t.Helper()
	s, err := snapshotstore.New(f.snapshots())
	require.NoError(t, err)
	return s
}

// disk is what the root disk of a tag holds: data at the start, the middle
// and the end of a file that is otherwise holes.
func disk(tag string) map[int64][]byte {
	return map[int64][]byte{0: []byte("head-" + tag), diskSize / 2: []byte("middle-" + tag), diskSize - 9: []byte("tail-" + tag)}
}

// files writes a Machine's files for a tag: a root disk and two state files.
func (f fixture) files(t *testing.T, tag string) machine.SnapshotFiles {
	t.Helper()
	run := filepath.Join(f.root, "run-"+tag)
	require.NoError(t, os.MkdirAll(run, 0o700))
	rootDisk := filepath.Join(f.root, "disk-"+tag)
	d, err := os.Create(rootDisk)
	require.NoError(t, err)
	require.NoError(t, d.Truncate(diskSize))
	for off, b := range disk(tag) {
		_, err := d.WriteAt(b, off)
		require.NoError(t, err)
	}
	require.NoError(t, d.Close())
	files := machine.SnapshotFiles{
		State:    []string{filepath.Join(run, "mem.snap"), filepath.Join(run, "vm.state")},
		RootDisk: rootDisk,
	}
	for _, p := range files.State {
		require.NoError(t, os.WriteFile(p, []byte(filepath.Base(p)+"-"+tag), 0o600))
	}
	return files
}

// runMarker stands for a secret in the run configuration of a snapshot.
const runMarker = "MARKER-VALUE-4f9c1e"

// snapshot is a record with every field set.
func snapshot(id string) machine.Snapshot {
	return machine.Snapshot{
		ID:      machine.SnapshotID(id),
		Machine: "machine-of-" + machine.MachineID(id),
		Request: "request-of-" + machine.RequestID(id),
		Spec: machine.SnapshotSpec{
			Image:     "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			Resources: machine.Resources{VCPUs: 3, MemoryBytes: 512 << 20},
			Ceiling:   machine.Resources{VCPUs: 6, MemoryBytes: 1 << 30},
			// Differs from the resources, so a reading that defaulted it would show.
			BootMemoryBytes: 256 << 20,
			RootDiskBytes:   diskSize,
			Command:         []string{"/bin/app", "--serve"},
			Args:            []string{"-v"},
			Env:             []machine.EnvVar{{Name: "TOKEN", Value: runMarker}, {Name: "MODE", Value: "x"}},
			User:            "7:8",
			Files:           []machine.PlacedFile{{Path: "/etc/token", Content: []byte(runMarker), Mode: 0o640, UID: 7, GID: 8}},
			WorkingDir:      "/srv/app",
			Probes: []machine.Probe{
				{Name: "ready", Kind: machine.ProbeHTTPGet, Port: 8080, Path: "/healthz", Scheme: "http", InitialDelay: 2 * time.Second, Period: 5 * time.Second, Timeout: 500 * time.Millisecond, SuccessThreshold: 2, FailureThreshold: 4},
				{Name: "listening", Kind: machine.ProbeTCP, Port: 5432},
				{Name: "alive", Kind: machine.ProbeExec, Argv: []string{"/bin/check", "--quick"}},
			},
		},
		RootDiskUUID: "uuid-of-" + id,
		CreatedAt:    time.Unix(1700000200, 987654321),
		MAC:          "02:00:00:00:00:01",
		Compat:       machine.SnapshotCompat{Driver: "firecracker", Version: "1.17.0", CPUModel: "model-x", KernelDigest: "sha256:kernel", KernelName: "next"},
		Disks: []machine.SnapshotDisk{
			{Disk: "data", SizeBytes: 1 << 20, ModTime: time.Unix(1700000000, 123456789)},
			{Disk: "tools", ReadOnly: true, MountPath: "/tools", SizeBytes: 2 << 20, ModTime: time.Unix(1700000100, 5)},
		},
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(b)
}

// requireWhole checks that the store holds the snapshot with the files of the
// tag, every one of them with its content.
func requireWhole(t *testing.T, s *snapshotstore.Store, id, tag string) {
	t.Helper()
	got, files, err := s.Get(ctx, machine.SnapshotID(id))
	require.NoError(t, err)
	assert.Equal(t, snapshot(id), got)
	require.Len(t, files.State, 2)
	assert.Equal(t, "mem.snap-"+tag, read(t, files.State[0]))
	assert.Equal(t, "vm.state-"+tag, read(t, files.State[1]))
	d, err := os.Open(files.RootDisk)
	require.NoError(t, err)
	defer d.Close()
	for off, want := range disk(tag) {
		b := make([]byte, len(want))
		_, err := d.ReadAt(b, off)
		require.NoError(t, err)
		assert.Equal(t, want, b, "the copy of the root disk at %d", off)
	}
}

// requireUntouched checks that the files a Put was given are where they were,
// with their content.
func requireUntouched(t *testing.T, files machine.SnapshotFiles, tag string) {
	t.Helper()
	for _, p := range files.State {
		assert.Equal(t, filepath.Base(p)+"-"+tag, read(t, p))
	}
	d, err := os.Open(files.RootDisk)
	require.NoError(t, err)
	defer d.Close()
	for off, want := range disk(tag) {
		b := make([]byte, len(want))
		_, err := d.ReadAt(b, off)
		require.NoError(t, err)
		assert.Equal(t, want, b)
	}
}

// requireNothingKept checks that the snapshots directory holds no snapshot
// and nothing in staging.
func requireNothingKept(t *testing.T, f fixture) {
	t.Helper()
	entries, err := os.ReadDir(f.snapshots())
	require.NoError(t, err)
	for _, e := range entries {
		require.Equal(t, "staging", e.Name(), "a snapshot is left")
	}
	left, err := os.ReadDir(filepath.Join(f.snapshots(), "staging"))
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestPutGet_ReturnsTheRecordAndTheFiles(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	require.NoError(t, s.Put(ctx, snapshot("a"), files))

	requireWhole(t, s, "a", "a")
	// RootDisk is copied: the Machine's own file is still there, and a
	// different file from the one Get returns.
	_, got, err := s.Get(ctx, "a")
	require.NoError(t, err)
	assert.NotEqual(t, files.RootDisk, got.RootDisk)
	requireUntouchedDisk(t, files, "a")
}

func requireUntouchedDisk(t *testing.T, files machine.SnapshotFiles, tag string) {
	t.Helper()
	files.State = nil
	requireUntouched(t, files, tag)
}

func TestPutGet_SurvivesARestart(t *testing.T) {
	f := newFixture(t)
	require.NoError(t, f.open(t).Put(ctx, snapshot("a"), f.files(t, "a")))

	requireWhole(t, f.open(t), "a", "a")
}

func TestPutGet_HoldsSeveralSnapshotsApart(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))

	requireWhole(t, s, "a", "a")
	requireWhole(t, s, "b", "b")
}

// A field added to machine.Snapshot that the record does not keep fails here:
// the literal below is checked to set every field, and the round trip to
// return every one.
func TestRecord_KeepsEveryFieldOfASnapshot(t *testing.T) {
	var unset func(path string, v reflect.Value)
	unset = func(path string, v reflect.Value) {
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				name := path + "." + v.Type().Field(i).Name
				unset(name, v.Field(i))
			}
		default:
			assert.False(t, v.IsZero(), "%s is zero in the test's snapshot, so the test cannot see it lost", path)
		}
	}
	want := snapshot("a")
	unset("Snapshot", reflect.ValueOf(want))

	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, want, f.files(t, "a")))
	got, _, err := s.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, want, got)
}

func TestPut_RefusesAnExistingIdentifier(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))

	// A second Put of "a" is refused and changes nothing.
	second := f.files(t, "second")
	var err error
	steps := recordSteps(s, func() { err = s.Put(ctx, snapshot("a"), second) })
	assert.ErrorIs(t, err, machine.ErrSnapshotExists)
	assert.Empty(t, steps, "the root disk was copied before the refusal")
	requireUntouched(t, second, "second")
	requireWhole(t, s, "a", "a")

	// Control: the same files under another identifier are accepted, so the
	// identifier was the reason.
	other := snapshot("b")
	require.NoError(t, s.Put(ctx, other, second))
}

func TestPut_TheLaterOfTwoPutsOfOneIdentifierIsRefused(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	first, second := f.files(t, "first"), f.files(t, "second")
	// The second Put publishes while the first is about to: only the rename
	// of the first can tell, and it must.
	inner := false
	s.SetFault(func(step string) error {
		if step == "publish" && !inner {
			inner = true
			assert.NoError(t, s.Put(ctx, snapshot("a"), second))
		}
		return nil
	})
	err := s.Put(ctx, snapshot("a"), first)
	assert.ErrorIs(t, err, machine.ErrSnapshotExists)
	requireUntouched(t, first, "first")
	requireWhole(t, s, "a", "second")
}

func TestPut_PutsOfOneIdentifierAtOnceKeepExactlyOne(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	const n = 8
	all := make([]machine.SnapshotFiles, n)
	for i := range all {
		all[i] = f.files(t, string(rune('a'+i)))
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.Put(ctx, snapshot("same"), all[i])
		}()
	}
	wg.Wait()
	won := 0
	for i, err := range errs {
		if err == nil {
			won++
			continue
		}
		assert.ErrorIs(t, err, machine.ErrSnapshotExists)
		requireUntouched(t, all[i], string(rune('a'+i)))
	}
	assert.Equal(t, 1, won)
	left, err := os.ReadDir(filepath.Join(f.snapshots(), "staging"))
	require.NoError(t, err)
	assert.Empty(t, left)
}

func TestPut_RefusesStateFilesOfOneName(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	other := filepath.Join(f.root, "other-run")
	require.NoError(t, os.MkdirAll(other, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(other, "mem.snap"), []byte("x"), 0o600))

	clash := files
	clash.State = []string{files.State[0], filepath.Join(other, "mem.snap")}
	assert.Error(t, s.Put(ctx, snapshot("a"), clash))
	requireUntouched(t, files, "a")
	requireNothingKept(t, f)

	// Control: the files under different names are accepted.
	require.NoError(t, s.Put(ctx, snapshot("a"), files))
}

func TestPut_FailsWithoutLeavingAnythingWhenAnInputIsMissing(t *testing.T) {
	cases := map[string]func(machine.SnapshotFiles) machine.SnapshotFiles{
		"the root disk is not there": func(fs machine.SnapshotFiles) machine.SnapshotFiles {
			fs.RootDisk += ".gone"
			return fs
		},
		"the root disk is a directory": func(fs machine.SnapshotFiles) machine.SnapshotFiles {
			fs.RootDisk = filepath.Dir(fs.RootDisk)
			return fs
		},
		"a state file is not there": func(fs machine.SnapshotFiles) machine.SnapshotFiles {
			fs.State = []string{fs.State[0], fs.State[1] + ".gone"}
			return fs
		},
		"a state file is a directory": func(fs machine.SnapshotFiles) machine.SnapshotFiles {
			fs.State = []string{fs.State[0], filepath.Dir(fs.State[1])}
			return fs
		},
	}
	for name, broken := range cases {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			files := f.files(t, "a")
			err := s.Put(ctx, snapshot("a"), broken(files))
			assert.Error(t, err)
			assert.NotErrorIs(t, err, machine.ErrSnapshotExists)
			requireUntouched(t, files, "a")
			requireNothingKept(t, f)

			// Control: the intact files are accepted.
			require.NoError(t, s.Put(ctx, snapshot("a"), files))
		})
	}
}

func TestPut_EndedContextStoresNothing(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	ended, cancel := context.WithCancel(ctx)
	cancel()

	assert.ErrorIs(t, s.Put(ended, snapshot("a"), files), context.Canceled)
	requireUntouched(t, files, "a")
	requireNothingKept(t, f)
	require.NoError(t, s.Put(ctx, snapshot("a"), files)) // control
}

func TestGet_RefusesAnAbsentSnapshot(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))

	_, _, err := s.Get(ctx, "b")
	assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
	// Control: the held one is found.
	_, _, err = s.Get(ctx, "a")
	assert.NoError(t, err)
}

func TestGet_DoesNotCallAnUnreadableRecordAbsent(t *testing.T) {
	// The record of "a" is written wrong in each way below, and "b" stays
	// right as the control.
	damage := map[string]func(dir string) error{
		"not JSON": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "record.json"), []byte("{not json"), 0o600)
		},
		"another version": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"version":99,"id":"a","state":[]}`), 0o600)
		},
		"a state name that leaves the directory": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"version":1,"id":"a","state":["../../x"]}`), 0o600)
		},
		"a record of another snapshot": func(dir string) error {
			return os.WriteFile(filepath.Join(dir, "record.json"), []byte(`{"version":1,"id":"z","state":[]}`), 0o600)
		},
	}
	for name, do := range damage {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
			require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))
			_, files, err := s.Get(ctx, "a")
			require.NoError(t, err)
			require.NoError(t, do(filepath.Dir(files.RootDisk)))

			_, _, err = s.Get(ctx, "a")
			assert.Error(t, err)
			assert.NotErrorIs(t, err, machine.ErrSnapshotAbsent)
			requireWhole(t, s, "b", "b")
		})
	}
}

func TestGet_EndedContextAnswersNothing(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	ended, cancel := context.WithCancel(ctx)
	cancel()

	_, _, err := s.Get(ended, "a")
	assert.ErrorIs(t, err, context.Canceled)
	_, _, err = s.Get(ctx, "a") // control
	assert.NoError(t, err)
}

func TestRemove_DeletesTheSnapshotAndItsFilesAndOnlyThose(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))
	_, held, err := s.Get(ctx, "a")
	require.NoError(t, err)

	require.NoError(t, s.Remove(ctx, "a"))

	_, _, err = s.Get(ctx, "a")
	assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
	for _, p := range append([]string{held.RootDisk}, held.State...) {
		assert.NoFileExists(t, p)
	}
	requireWhole(t, s, "b", "b") // control: another snapshot is kept
	left, err := os.ReadDir(filepath.Join(f.snapshots(), "staging"))
	require.NoError(t, err)
	assert.Empty(t, left)
	// The identifier is free again.
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "again")))
}

func TestRemove_AbsentSucceeds(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))

	assert.NoError(t, s.Remove(ctx, "a"))
	requireWhole(t, s, "b", "b")
}

func TestRemove_EndedContextRemovesNothing(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	ended, cancel := context.WithCancel(ctx)
	cancel()

	assert.ErrorIs(t, s.Remove(ended, "a"), context.Canceled)
	requireWhole(t, s, "a", "a")
	require.NoError(t, s.Remove(ctx, "a")) // control
	_, _, err := s.Get(ctx, "a")
	assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
}

// A record written before the ceiling and the boot memory existed holds
// neither; it reads back with the ceiling the resources are, and the memory
// they boot with (ADR 0029).
func TestGet_ARecordWithoutACeilingReadsItAsTheResources(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	old := snapshot("old")
	old.Spec.Ceiling = machine.Resources{}
	old.Spec.BootMemoryBytes = 0
	require.NoError(t, s.Put(ctx, old, f.files(t, "old")))

	got, _, err := s.Get(ctx, "old")
	require.NoError(t, err)
	assert.Equal(t, old.Spec.Resources, got.Spec.Ceiling)
	assert.Equal(t, old.Spec.Resources.MemoryBytes, got.Spec.BootMemoryBytes)

	kept := snapshot("kept")
	require.NoError(t, s.Put(ctx, kept, f.files(t, "kept")))
	got, _, err = s.Get(ctx, "kept")
	require.NoError(t, err)
	assert.Equal(t, kept.Spec.Ceiling, got.Spec.Ceiling, "control: one that holds a ceiling keeps it")
	assert.Equal(t, kept.Spec.BootMemoryBytes, got.Spec.BootMemoryBytes)
}

// A record written before disks were recorded holds none; it reads back with
// none, and one that holds disks keeps each with its flag, size and time.
func TestGet_ARecordWithoutDisksReadsAsNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	old := snapshot("old")
	old.Disks = nil
	require.NoError(t, s.Put(ctx, old, f.files(t, "old")))
	kept := snapshot("kept")
	require.NoError(t, s.Put(ctx, kept, f.files(t, "kept")))

	got, _, err := s.Get(ctx, "old")
	require.NoError(t, err)
	assert.Empty(t, got.Disks)

	got, _, err = s.Get(ctx, "kept")
	require.NoError(t, err)
	assert.Equal(t, kept.Disks, got.Disks, "control: one that holds disks keeps them")
}

// A record written before kernels had names reads back with none, and the
// name of a record written now is kept.
func TestGet_ARecordWithoutAKernelNameReadsBackWithNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	got, files, err := s.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, "next", got.Compat.KernelName, "control: a record written now keeps the name")

	path := filepath.Join(filepath.Dir(files.RootDisk), "record.json")
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(read(t, path)), &fields))
	require.Contains(t, fields, "kernel_name")
	delete(fields, "kernel_name")
	legacy, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, legacy, 0o600))

	got, _, err = f.open(t).Get(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, got.Compat.KernelName)
	assert.Equal(t, "sha256:kernel", got.Compat.KernelDigest)
}

// A record written before the run configuration was kept reads back with none
// of it, and the record of a snapshot written now keeps all six fields in the
// owner-only file and nowhere else.
func TestGet_ARecordWithoutARunConfigurationReadsBackWithNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	got, files, err := s.Get(ctx, "a")
	require.NoError(t, err)
	assert.Equal(t, snapshot("a").Spec, got.Spec, "control: a record written now keeps them")

	dir := filepath.Dir(files.RootDisk)
	path := filepath.Join(dir, "record.json")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "the record is the daemon's alone")
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(read(t, path)), &fields))
	for _, key := range []string{"command", "args", "env", "run_user", "files", "working_dir", "probes"} {
		require.Contains(t, fields, key)
		delete(fields, key)
	}
	legacy, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, legacy, 0o600))

	got, _, err = f.open(t).Get(ctx, "a")
	require.NoError(t, err)
	assert.Empty(t, got.Spec.Command)
	assert.Empty(t, got.Spec.Args)
	assert.Empty(t, got.Spec.Env)
	assert.Empty(t, got.Spec.User)
	assert.Empty(t, got.Spec.Files)
	assert.Empty(t, got.Spec.WorkingDir)
	assert.Empty(t, got.Spec.Probes)
	assert.Equal(t, "sha256:kernel", got.Compat.KernelDigest, "the rest of the record reads as before")
}
