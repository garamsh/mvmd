package snapshotstore_test

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/snapshotstore"
)

var errInjected = errors.New("injected failure")

// putSteps are the steps a Put of two state files asks the seam about, in
// order. The test below fails when Put gains or loses a step, so that no step
// goes untested.
var putSteps = []string{"stage", "copy-disk", "write-record", "move-state", "move-state", "sync-state", "publish", "sync-dir"}

var removeSteps = []string{"retire", "sync-dir", "delete"}

// recordSteps runs do with a fault that fails nothing and returns the steps
// it was asked about.
func recordSteps(s *snapshotstore.Store, do func()) []string {
	var steps []string
	s.SetFault(func(step string) error {
		steps = append(steps, step)
		return nil
	})
	do()
	s.SetFault(nil)
	return steps
}

func TestPut_AsksTheSeamAboutEveryStep(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	steps := recordSteps(s, func() { require.NoError(t, s.Put(ctx, snapshot("a"), files)) })
	assert.Equal(t, putSteps, steps)
}

func TestRemove_AsksTheSeamAboutEveryStep(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	steps := recordSteps(s, func() { require.NoError(t, s.Remove(ctx, "a")) })
	assert.Equal(t, removeSteps, steps)
}

// failAt makes the nth step the seam is asked about fail.
func failAt(s *snapshotstore.Store, n int) {
	i := 0
	s.SetFault(func(string) error {
		i++
		if i == n {
			return errInjected
		}
		return nil
	})
}

func TestPut_AFailureAtAnyStepLeavesNothingAndTheFilesWhereTheyWere(t *testing.T) {
	for n, step := range putSteps {
		t.Run(step+"-"+string(rune('0'+n)), func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			files := f.files(t, "a")
			failAt(s, n+1)

			err := s.Put(ctx, snapshot("a"), files)
			assert.ErrorIs(t, err, errInjected)

			_, _, getErr := s.Get(ctx, "a")
			assert.ErrorIs(t, getErr, machine.ErrSnapshotAbsent)
			requireUntouched(t, files, "a")
			requireNothingKept(t, f)

			// Control: with no failure the same call keeps the snapshot, so
			// the failure was the reason nothing was kept.
			s.SetFault(nil)
			require.NoError(t, s.Put(ctx, snapshot("a"), files))
			requireWhole(t, s, "a", "a")
		})
	}
}

func TestPut_AStateFileThatCannotBeReturnedIsReportedAndNotDeleted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Fatal("this test needs a user that a directory's mode binds, and the tests run as root")
	}
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	// The failure comes after both state files were taken in. The run
	// directory is then made unwritable, so neither goes back.
	run := filepath.Dir(files.State[0])
	s.SetFault(func(step string) error {
		if step != "sync-state" {
			return nil
		}
		require.NoError(t, os.Chmod(run, 0o500))
		return errInjected
	})

	err := s.Put(ctx, snapshot("a"), files)
	require.NoError(t, os.Chmod(run, 0o700))
	assert.ErrorIs(t, err, errInjected)
	assert.Contains(t, err.Error(), "return the state files")
	// What was taken in is still on disk until the next New, which is where
	// the Driver's files are not deleted by the failed Put itself.
	staged, readErr := os.ReadDir(filepath.Join(f.snapshots(), "staging"))
	require.NoError(t, readErr)
	assert.Len(t, staged, 1)
}

func TestPut_AFailureAfterThePublishHidesTheSnapshotAgain(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	var seen error
	s.SetFault(func(step string) error {
		if step == "sync-dir" {
			// The snapshot is visible here, as a crash after the rename leaves it.
			_, _, seen = s.Get(ctx, "a")
			return errInjected
		}
		return nil
	})

	require.ErrorIs(t, s.Put(ctx, snapshot("a"), files), errInjected)
	assert.NoError(t, seen, "the snapshot was published when the step failed")
	_, _, err := s.Get(ctx, "a")
	assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
	requireUntouched(t, files, "a")
	requireNothingKept(t, f)
}

func TestRemove_AFailureAtAnyStep(t *testing.T) {
	for n, step := range removeSteps {
		t.Run(step, func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
			require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))
			failAt(s, n+1)

			assert.ErrorIs(t, s.Remove(ctx, "a"), errInjected)

			// Before the rename the snapshot is whole; after it, it is gone and
			// what is left is reclaimed. Never half.
			_, _, err := s.Get(ctx, "a")
			if step == "retire" {
				assert.NoError(t, err)
				requireWhole(t, s, "a", "a")
			} else {
				assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
			}
			requireWhole(t, s, "b", "b")

			// A restart reclaims the leftovers, and a Remove that is repeated
			// without the failure finishes the job.
			s.SetFault(nil)
			require.NoError(t, s.Remove(ctx, "a"))
			s = f.open(t)
			_, _, err = s.Get(ctx, "a")
			assert.ErrorIs(t, err, machine.ErrSnapshotAbsent)
			requireWhole(t, s, "b", "b")
			left, err := os.ReadDir(filepath.Join(f.snapshots(), "staging"))
			require.NoError(t, err)
			assert.Empty(t, left)
		})
	}
}

// copyTree copies a directory tree of small files, the state of the disk at
// one moment.
func copyTree(t *testing.T, from, to string) {
	t.Helper()
	require.NoError(t, filepath.WalkDir(from, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(from, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(to, rel)
		if d.IsDir() {
			return os.MkdirAll(dst, 0o700)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		return errors.Join(err, out.Close())
	}))
}

// crashPoints runs do and returns a copy of the snapshots directory as it was
// before each step the seam was asked about, and after do: the disk a crash
// at that moment leaves.
func crashPoints(t *testing.T, f fixture, s *snapshotstore.Store, do func()) []string {
	t.Helper()
	var points []string
	take := func() {
		dst := filepath.Join(t.TempDir(), "snapshots")
		copyTree(t, f.snapshots(), dst)
		points = append(points, dst)
	}
	s.SetFault(func(string) error {
		take()
		return nil
	})
	do()
	s.SetFault(nil)
	take()
	return points
}

func TestPut_ACrashAtAnyStepLeavesTheWholeSnapshotOrNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	points := crashPoints(t, f, s, func() { require.NoError(t, s.Put(ctx, snapshot("a"), files)) })
	require.Len(t, points, len(putSteps)+1)

	for i, dir := range points {
		recovered, err := snapshotstore.New(dir)
		require.NoError(t, err)
		_, _, getErr := recovered.Get(ctx, "a")
		if i < len(putSteps) {
			// Before the step "publish" asked about, and so before the rename.
			published := i > indexOf(putSteps, "publish")
			if published {
				require.NoError(t, getErr, "crash before %s", putSteps[i])
				requireWhole(t, recovered, "a", "a")
			} else {
				assert.ErrorIs(t, getErr, machine.ErrSnapshotAbsent, "crash before %s", putSteps[i])
			}
		} else {
			require.NoError(t, getErr, "crash after the Put")
			requireWhole(t, recovered, "a", "a")
		}
		// What the crash left is gone after the restart, the snapshot excepted.
		left, err := os.ReadDir(filepath.Join(dir, "staging"))
		require.NoError(t, err)
		assert.Empty(t, left, "after a crash before step %d", i)
		// And the identifier works: it is taken, or it takes a new Put.
		if getErr != nil {
			require.NoError(t, recovered.Put(ctx, snapshot("a"), f.files(t, "again")))
		}
	}
}

func indexOf(list []string, s string) int {
	for i, v := range list {
		if v == s {
			return i
		}
	}
	return -1
}

func TestRemove_ACrashAtAnyStepLeavesTheWholeSnapshotOrNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))
	points := crashPoints(t, f, s, func() { require.NoError(t, s.Remove(ctx, "a")) })
	require.Len(t, points, len(removeSteps)+1)

	for i, dir := range points {
		recovered, err := snapshotstore.New(dir)
		require.NoError(t, err)
		_, _, getErr := recovered.Get(ctx, "a")
		if i == 0 { // before the rename
			require.NoError(t, getErr)
			requireWhole(t, recovered, "a", "a")
		} else {
			assert.ErrorIs(t, getErr, machine.ErrSnapshotAbsent, "crash before step %d", i)
		}
		requireWhole(t, recovered, "b", "b")
		left, err := os.ReadDir(filepath.Join(dir, "staging"))
		require.NoError(t, err)
		assert.Empty(t, left, "after a crash before step %d", i)
	}
}

func TestNew_ReclaimsWhatIsInStagingAndKeepsTheSnapshots(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	staging := filepath.Join(f.snapshots(), "staging")
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "put-x", "state"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "put-x", "state", "mem.snap"), []byte("half"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(staging, "removed-y"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(staging, "stray"), []byte("x"), 0o600))
	// Setup is what the test says it is.
	left, err := os.ReadDir(staging)
	require.NoError(t, err)
	require.Len(t, left, 3)

	s = f.open(t)

	left, err = os.ReadDir(staging)
	require.NoError(t, err)
	assert.Empty(t, left)
	requireWhole(t, s, "a", "a")
}
