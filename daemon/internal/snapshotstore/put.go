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
// and its name there. was is its owner and mode as the Driver left them, once
// the store has set its own.
type moved struct {
	from, name string
	was        *fileState
}

// fileState is an owner and a mode.
type fileState struct {
	uid, gid int
	mode     os.FileMode
}

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
		p.taken = append(p.taken, moved{from: from, name: names[i]})
		if info, err := os.Lstat(to); err != nil {
			return err
		} else if !info.Mode().IsRegular() {
			return fmt.Errorf("state file %s is not a regular file", from)
		}
	}
	if err := s.step("own-state"); err != nil {
		return err
	}
	for i, name := range names {
		if err := p.ownState(i, filepath.Join(stateDir, name)); err != nil {
			return fmt.Errorf("give the state file %s to the store: %w", name, err)
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
// state files to where the Driver wrote them, with the owner and mode it gave
// them, and removes what was built. The
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
		renameErr := os.Rename(filepath.Join(dir, stateName, t.name), t.from)
		errs = append(errs, renameErr)
		if renameErr == nil && t.was != nil {
			errs = append(errs, setState(t.from, *t.was))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return fmt.Errorf("return the state files of a failed Put: %w", err)
	}
	return os.RemoveAll(dir)
}

// stateMode is the mode of a published state file: a restoring Machine's VMM
// runs as a user of its own and has to read it (ADR 0031), and a published
// snapshot is never written again.
const stateMode = 0o444

// ownState makes the state file at path, the i-th taken, the store's: owned by
// this process's user and group, mode stateMode. It notes the owner and mode
// the Driver left, which undo returns. The origin Machine's user, who wrote
// the file, no longer owns it afterwards.
func (p *put) ownState(i int, path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("the owner of %s is not readable", path)
	}
	p.taken[i].was = &fileState{int(st.Uid), int(st.Gid), info.Mode().Perm()}
	return setState(path, fileState{os.Geteuid(), os.Getegid(), stateMode})
}

// setState gives the file at path the owner and mode, without following a
// link.
func setState(path string, to fileState) error {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	if err := f.Chown(to.uid, to.gid); err != nil {
		return errors.Join(err, f.Close())
	}
	return errors.Join(f.Chmod(to.mode), f.Close())
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
