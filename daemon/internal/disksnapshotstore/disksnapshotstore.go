// Package disksnapshotstore is mvmd's store of disk snapshots: each snapshot is
// one directory of the node's disk-snapshots directory, holding a copy of a
// disk's file and its record, and it becomes visible by one rename of a
// directory built outside the snapshots (ADR 0056). The directory lies on the
// file system of the disks directory, so a copy is a reflink where that file
// system has one.
package disksnapshotstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

var _ machine.DiskSnapshotStore = (*Store)(nil)

const (
	stagingName = "staging"
	recordName  = "record.json"
	diskName    = "disk"
	// blockBytes is the unit of st_blocks, whatever the file system's block size.
	blockBytes = 512
)

// Store is the disk snapshots of a node in one directory. A snapshot is the
// directory named by the SHA-256 of its identifier, so any identifier is a safe
// name. Put, Get, List and Remove are safe to call at once, for the same
// snapshot too: what decides a race is a rename. One process holds the
// directory at a time.
type Store struct {
	dir string

	// fault is the seam of the tests: it is asked before each step of Put and
	// Remove and may fail it. It is nil in production.
	fault func(step string) error
	// clone is the clone Put and Duplicate try; nil is sparsecopy.Clone. A test
	// replaces it to stand for a file system with reflinks, or without.
	clone sparsecopy.Cloner
}

// New opens the disk snapshots in dir, creating it when it is not there, and
// removes what a crash left in dir's staging area: a Put that had not
// published, and a Remove that had not finished deleting. Nothing else uses the
// directory yet, so nothing in staging is in use.
func New(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(s.stagingDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create the disk snapshot store: %w", err)
	}
	if err := syncDir(dir); err != nil {
		return nil, fmt.Errorf("create the disk snapshot store: %w", err)
	}
	if err := syncDir(filepath.Dir(dir)); err != nil {
		return nil, fmt.Errorf("create the disk snapshot store: %w", err)
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

// snapshotPath is the directory of a snapshot. No hex name is "staging".
func (s *Store) snapshotPath(id machine.DiskSnapshotID) string {
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

// record is the JSON of a snapshot's record.
type record struct {
	ID         string    `json:"id"`
	Disk       string    `json:"disk"`
	Request    string    `json:"request"`
	SizeBytes  uint64    `json:"size_bytes"`
	Filesystem string    `json:"filesystem"`
	CreatedAt  time.Time `json:"created_at"`
}

func encodeRecord(d machine.DiskSnapshot) ([]byte, error) {
	return json.Marshal(record{ID: string(d.ID), Disk: string(d.Disk), Request: string(d.Request), SizeBytes: d.SizeBytes, Filesystem: string(d.Filesystem), CreatedAt: d.CreatedAt})
}

func decodeRecord(raw []byte) (machine.DiskSnapshot, error) {
	var r record
	if err := json.Unmarshal(raw, &r); err != nil {
		return machine.DiskSnapshot{}, err
	}
	return machine.DiskSnapshot{ID: machine.DiskSnapshotID(r.ID), Disk: machine.DiskID(r.Disk), Request: machine.RequestID(r.Request), SizeBytes: r.SizeBytes, Filesystem: machine.Filesystem(r.Filesystem), CreatedAt: r.CreatedAt}, nil
}

// Put implements machine.DiskSnapshotStore. The snapshot is built in a
// directory of staging and published by renaming it to the snapshot's own name,
// so a crash leaves the whole snapshot or none, and what it leaves in staging
// is reclaimed by New. A failure that is not a crash removes what it built.
func (s *Store) Put(ctx context.Context, snapshot machine.DiskSnapshot, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	final := s.snapshotPath(snapshot.ID)
	// A cheap answer before the disk is copied; the rename below is the one that
	// decides.
	if _, err := os.Lstat(final); err == nil {
		return machine.ErrDiskSnapshotExists
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	content, err := encodeRecord(snapshot)
	if err != nil {
		return err
	}
	stage, err := s.newStagingPath("put-")
	if err != nil {
		return err
	}
	if err := s.build(stage, final, source, content); err != nil {
		return errors.Join(err, os.RemoveAll(stage))
	}
	return nil
}

func (s *Store) build(stage, final, source string, content []byte) error {
	if err := s.step("stage"); err != nil {
		return err
	}
	if err := os.Mkdir(stage, 0o700); err != nil {
		return err
	}
	if err := s.step("copy-disk"); err != nil {
		return err
	}
	if err := s.duplicate(source, filepath.Join(stage, diskName)); err != nil {
		return fmt.Errorf("copy the disk: %w", err)
	}
	if err := s.step("write-record"); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(stage, recordName), content); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	if err := s.step("publish"); err != nil {
		return err
	}
	// A directory is replaced only when empty, and a published one never is, so
	// the later of two Puts of one identifier fails here.
	switch err := os.Rename(stage, final); {
	case errors.Is(err, syscall.ENOTEMPTY), errors.Is(err, syscall.EEXIST):
		return machine.ErrDiskSnapshotExists
	case err != nil:
		return err
	}
	if err := syncDir(s.dir); err != nil {
		// The snapshot is visible and may not be durable: it does not stay.
		return errors.Join(err, os.RemoveAll(final))
	}
	return nil
}

// duplicate makes the file at to a copy of the regular file at from that shares
// no later change with it.
func (s *Store) duplicate(from, to string) error {
	src, err := os.Open(from)
	if err != nil {
		return err
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", from)
	}
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	clone := s.clone
	if clone == nil {
		clone = sparsecopy.Clone
	}
	if _, err := sparsecopy.DuplicateWith(clone, dst, src); err != nil {
		return errors.Join(err, dst.Close())
	}
	if err := dst.Sync(); err != nil {
		return errors.Join(err, dst.Close())
	}
	return dst.Close()
}

// Get implements machine.DiskSnapshotStore.
func (s *Store) Get(ctx context.Context, id machine.DiskSnapshotID) (machine.DiskSnapshot, string, error) {
	if err := ctx.Err(); err != nil {
		return machine.DiskSnapshot{}, "", err
	}
	dir := s.snapshotPath(id)
	raw, err := os.ReadFile(filepath.Join(dir, recordName))
	if errors.Is(err, os.ErrNotExist) {
		return machine.DiskSnapshot{}, "", machine.ErrDiskSnapshotAbsent
	}
	if err != nil {
		return machine.DiskSnapshot{}, "", err
	}
	snapshot, err := decodeRecord(raw)
	if err != nil {
		return machine.DiskSnapshot{}, "", fmt.Errorf("disk snapshot %q: read the record: %w", id, err)
	}
	if snapshot.ID != id {
		return machine.DiskSnapshot{}, "", fmt.Errorf("disk snapshot %q: the record in %s is of %q", id, dir, snapshot.ID)
	}
	return snapshot, filepath.Join(dir, diskName), nil
}

// List implements machine.DiskSnapshotStore. The staging directory holds no
// record, so it is left out, as is a snapshot retired between the listing of the
// directory and the read of its record or its measurement: that one is gone. A
// record that cannot be read fails the listing, since a node holding one is not
// in a state a client can reconcile against.
func (s *Store) List(ctx context.Context) ([]machine.DiskSnapshotInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	out := make([]machine.DiskSnapshotInfo, 0, len(entries))
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name(), recordName))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		snapshot, err := decodeRecord(raw)
		if err != nil {
			return nil, fmt.Errorf("read the record in %s: %w", e.Name(), err)
		}
		size, err := occupied(filepath.Join(s.dir, e.Name()))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("measure the disk snapshot in %s: %w", e.Name(), err)
		}
		out = append(out, machine.DiskSnapshotInfo{DiskSnapshot: snapshot, AllocatedBytes: size})
	}
	slices.SortFunc(out, func(a, b machine.DiskSnapshotInfo) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

// Remove implements machine.DiskSnapshotStore. The snapshot stops being visible
// in one rename, into staging, and its files are deleted from there: a failure
// of the deletion returns its error with the snapshot already gone, and the
// next New reclaims the files.
func (s *Store) Remove(ctx context.Context, id machine.DiskSnapshotID) error {
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

// occupied returns the disk space the regular files under dir occupy: allocated
// blocks, so a sparse or cloned file counts what it holds and not its length.
func occupied(dir string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("the allocation of %s is not readable", path)
		}
		total += uint64(st.Blocks) * blockBytes
		return nil
	})
	return total, err
}

// writeFileSync creates the file with content and syncs it. The file must not
// exist.
func writeFileSync(path string, content []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(content); err != nil {
		return errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// syncDir flushes the directory's entries, which a sync of the files in it does
// not (fsync(2)).
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
