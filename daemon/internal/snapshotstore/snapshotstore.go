// Package snapshotstore is mvmd's Snapshot Store: each snapshot is one
// directory of a node's snapshots directory, holding a copy of the root disk,
// the Driver's state files and the record, and it becomes visible by one
// rename of a directory built outside the snapshots (ADR 0009 makes a
// snapshot a disk and memory pair).
package snapshotstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ machine.SnapshotStore = (*Store)(nil)

const (
	stagingName = "staging"
	recordName  = "record.json"
	diskName    = "root.disk"
	stateName   = "state"
)

// Store is the snapshots of a node in one directory. A snapshot is the
// directory named by the SHA-256 of its identifier, so any identifier is a
// safe name. Put, Get and Remove are safe to call at once, for the same
// snapshot too: what decides a race is a rename. One process holds the
// directory at a time.
type Store struct {
	dir string

	// fault is the seam of the tests: it is asked before each step of Put
	// and Remove and may fail it. It is nil in production.
	fault func(step string) error
}

// New opens the snapshots in dir, creating it when it is not there, and
// removes what a crash left in dir's staging area: a Put that had not
// published, and a Remove that had not finished deleting. Nothing else uses
// the directory yet, so nothing in staging is in use.
func New(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(s.stagingDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create the snapshot store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("create the snapshot store: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, fmt.Errorf("create the snapshot store: %w", err)
	}
	left, err := os.ReadDir(s.stagingDir())
	if err != nil {
		return nil, fmt.Errorf("reclaim what a crash left: %w", err)
	}
	for _, e := range left {
		if err := os.RemoveAll(filepath.Join(s.stagingDir(), e.Name())); err != nil {
			return nil, fmt.Errorf("reclaim what a crash left: %w", err)
		}
	}
	if err := syncDir(s.stagingDir()); err != nil {
		return nil, fmt.Errorf("reclaim what a crash left: %w", err)
	}
	return s, nil
}

func (s *Store) stagingDir() string { return filepath.Join(s.dir, stagingName) }

func (s *Store) snapshotPath(id machine.SnapshotID) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, hex.EncodeToString(sum[:]))
}

// step asks the test seam, when there is one, whether the step fails.
func (s *Store) step(name string) error {
	if s.fault == nil {
		return nil
	}
	return s.fault(name)
}

// newStagingPath returns the path of a new, unused name in staging.
func (s *Store) newStagingPath(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return filepath.Join(s.stagingDir(), prefix+hex.EncodeToString(raw)), nil
}

// Get implements machine.SnapshotStore.
func (s *Store) Get(ctx context.Context, id machine.SnapshotID) (machine.Snapshot, machine.SnapshotFiles, error) {
	if err := ctx.Err(); err != nil {
		return machine.Snapshot{}, machine.SnapshotFiles{}, err
	}
	dir := s.snapshotPath(id)
	raw, err := os.ReadFile(filepath.Join(dir, recordName))
	if errors.Is(err, os.ErrNotExist) {
		return machine.Snapshot{}, machine.SnapshotFiles{}, machine.ErrSnapshotAbsent
	}
	if err != nil {
		return machine.Snapshot{}, machine.SnapshotFiles{}, err
	}
	snapshot, names, err := decodeRecord(raw)
	if err != nil {
		return machine.Snapshot{}, machine.SnapshotFiles{}, fmt.Errorf("snapshot %q: read the record: %w", id, err)
	}
	if snapshot.ID != id {
		return machine.Snapshot{}, machine.SnapshotFiles{}, fmt.Errorf("snapshot %q: the record in %s is of %q", id, dir, snapshot.ID)
	}
	files := machine.SnapshotFiles{RootDisk: filepath.Join(dir, diskName)}
	for _, name := range names {
		files.State = append(files.State, filepath.Join(dir, stateName, name))
	}
	return snapshot, files, nil
}

// Remove implements machine.SnapshotStore. The snapshot stops being visible
// in one rename, into staging, and its files are deleted from there: a
// failure of the deletion returns its error with the snapshot already gone,
// and the next New reclaims the files.
func (s *Store) Remove(ctx context.Context, id machine.SnapshotID) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.step("retire"); err != nil {
		return err
	}
	retired, err := s.newStagingPath("removed-")
	if err != nil {
		return err
	}
	switch err := os.Rename(s.snapshotPath(id), retired); {
	case errors.Is(err, os.ErrNotExist):
		// Absent, or retired by another Remove.
	case err != nil:
		return err
	}
	if err := s.step("sync-dir"); err != nil {
		return err
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	if err := s.step("delete"); err != nil {
		return err
	}
	return os.RemoveAll(retired)
}
