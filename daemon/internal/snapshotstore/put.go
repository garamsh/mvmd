package snapshotstore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

// moved is a state file taken into the snapshot: where the Driver wrote it,
// and its name there.
type moved struct{ from, name string }

// Put implements machine.SnapshotStore. The snapshot is built in a directory
// of staging and published by renaming it to the snapshot's own name, so a
// crash leaves the whole snapshot or none, and what it leaves in staging is
// reclaimed by New. A failure that is not a crash undoes what was done,
// including moving the state files back.
func (s *Store) Put(ctx context.Context, snapshot machine.Snapshot, files machine.SnapshotFiles) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	names, err := stateNames(files.State)
	if err != nil {
		return err
	}
	final := s.snapshotPath(snapshot.ID)
	// A cheap answer before the disk is copied; the rename below is the one
	// that decides.
	if _, err := os.Lstat(final); err == nil {
		return machine.ErrSnapshotExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content, err := encodeRecord(snapshot, names)
	if err != nil {
		return err
	}

	stage, err := s.newStagingPath("put-")
	if err != nil {
		return err
	}
	p := &put{store: s, stage: stage, final: final}
	if err := p.build(files, names, content); err != nil {
		return errors.Join(err, p.undo())
	}
	return nil
}

// put is one Put in progress.
type put struct {
	store        *Store
	stage, final string
	taken        []moved
	published    bool
}

func (p *put) build(files machine.SnapshotFiles, names []string, content []byte) error {
	s := p.store
	stateDir := filepath.Join(p.stage, stateName)
	if err := s.step("stage"); err != nil {
		return err
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	if err := s.step("copy-disk"); err != nil {
		return err
	}
	if err := copyRootDisk(files.RootDisk, filepath.Join(p.stage, diskName)); err != nil {
		return fmt.Errorf("copy the root disk: %w", err)
	}
	if err := s.step("write-record"); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(p.stage, recordName), content); err != nil {
		return err
	}
	for i, from := range files.State {
		if err := s.step("move-state"); err != nil {
			return err
		}
		to := filepath.Join(stateDir, names[i])
		if err := os.Rename(from, to); err != nil {
			return fmt.Errorf("take in the state file: %w", err)
		}
		p.taken = append(p.taken, moved{from, names[i]})
		if info, err := os.Lstat(to); err != nil {
			return err
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("state file %s is not a regular file", from)
		}
	}
	if err := s.step("sync-state"); err != nil {
		return err
	}
	for _, name := range names {
		if err := syncFile(filepath.Join(stateDir, name)); err != nil {
			return err
		}
	}
	if err := syncDir(stateDir); err != nil {
		return err
	}
	if err := syncDir(p.stage); err != nil {
		return err
	}
	if err := s.step("publish"); err != nil {
		return err
	}
	// A directory is replaced only when empty, and a published one never is,
	// so the later of two Puts of one identifier fails here.
	switch err := os.Rename(p.stage, p.final); {
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return machine.ErrSnapshotExists
	case err != nil:
		return err
	}
	p.published = true
	if err := s.step("sync-dir"); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// undo takes a failed Put back: it hides a published snapshot, returns the
// state files to where the Driver wrote them and removes what was built. The
// build is left to the next New when a state file could not be returned,
// since deleting it would delete the Driver's file.
func (p *put) undo() error {
	dir := p.stage
	if p.published {
		if err := os.Rename(p.final, p.stage); err != nil {
			return fmt.Errorf("hide the snapshot of a failed Put: %w", err)
		}
	}
	var errs []error
	for i := len(p.taken) - 1; i >= 0; i-- {
		t := p.taken[i]
		errs = append(errs, os.Rename(filepath.Join(dir, stateName, t.name), t.from))
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("return the state files of a failed Put: %w", err)
	}
	return os.RemoveAll(dir)
}

// stateNames returns the name each state file takes inside the snapshot, its
// own, and refuses two files of one name.
func stateNames(paths []string) ([]string, error) {
	seen := make(map[string]bool, len(paths))
	names := make([]string, 0, len(paths))
	for _, path := range paths {
		name := filepath.Base(path)
		if err := checkName(name); err != nil {
			return nil, err
		}
		if seen[name] {
			return nil, fmt.Errorf("two state files are named %q", name)
		}
		seen[name] = true
		names = append(names, name)
	}
	return names, nil
}

// copyRootDisk copies the file at from to a new file at to, sparse, and syncs
// it. It reads from and never changes the source, and removes nothing on
// failure: the caller removes the staging directory.
func copyRootDisk(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	info, err := src.Stat()
	if err != nil {
		return errors.Join(err, src.Close())
	}
	if !info.Mode().IsRegular() {
		return errors.Join(fmt.Errorf("%s is not a regular file", from), src.Close())
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.Join(err, src.Close())
	}
	if err := sparsecopy.Copy(dst, src); err != nil {
		return errors.Join(err, src.Close(), dst.Close())
	}
	return errors.Join(dst.Sync(), src.Close(), dst.Close())
}
