// Package bootsetstore is mvmd's store of the boot sets a client added (ADR
// 0063): each is one directory of the node's boot-sets directory, holding the
// kernel and the initrd read-only and the digests both were added with. A set
// becomes visible by one rename of a directory built outside the boot sets, so
// a crash leaves a whole set or none.
package bootsetstore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ machine.BootSetStore = (*Store)(nil)

const (
	// stagingName is not a valid boot set name, so no set can collide with it.
	stagingName = ".staging"
	metaName    = "meta.json"
	kernelName  = "kernel"
	initrdName  = "initrd"
)

var validName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// Store is the added boot sets of a node in one directory. Add, Get, List and
// Remove are safe to call at once, for the same name too: what decides a race
// is a rename. One process holds the directory at a time.
type Store struct {
	dir string
}

// meta is the record of a set: the digests its files were added with.
type meta struct {
	KernelSHA256 string `json:"kernel_sha256"`
	InitrdSHA256 string `json:"initrd_sha256"`
}

// New opens the boot sets in dir, creating it when it is not there, and removes
// what a crash left in its staging area: an Add that had not published and a
// Remove that had not finished deleting. Nothing else uses the directory yet.
func New(dir string) (*Store, error) {
	s := &Store{dir: dir}
	if err := os.MkdirAll(s.stagingDir(), 0o700); err != nil {
		return nil, fmt.Errorf("create the boot set store: %w", err)
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
	if sets, err := os.ReadDir(dir); err == nil {
		for _, e := range sets {
			if e.Name()[0] != '.' {
				_ = os.RemoveAll(s.setPath(e.Name()))
			}
		}
	}
	for _, d := range []string{s.stagingDir(), dir, filepath.Dir(dir)} {
		if err := syncDir(d); err != nil {
			return nil, fmt.Errorf("create the boot set store: %w", err)
		}
	}
	return s, nil
}

func (s *Store) stagingDir() string { return filepath.Join(s.dir, stagingName) }

func (s *Store) setPath(name string) string { return filepath.Join(s.dir, name) }

// newStagingPath returns the path of a new, unused name in staging.
func (s *Store) newStagingPath(prefix string) (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return filepath.Join(s.stagingDir(), prefix+hex.EncodeToString(raw)), nil
}

// Add implements machine.BootSetStore. Each file is hashed as it is written,
// and compared with its declared digest only when its last byte has arrived.
func (s *Store) Add(ctx context.Context, name string, upload machine.BootUpload) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("boot set name %q is not valid", name)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	staged, err := s.newStagingPath("add-")
	if err != nil {
		return err
	}
	if err := os.Mkdir(staged, 0o700); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staged)
		}
	}()
	for _, f := range []struct {
		file    string
		content machine.BootContent
	}{{kernelName, upload.Kernel}, {initrdName, upload.Initrd}} {
		if err := writeVerified(ctx, filepath.Join(staged, f.file), f.file, f.content); err != nil {
			return err
		}
	}
	record, err := json.Marshal(meta{KernelSHA256: upload.Kernel.SHA256, InitrdSHA256: upload.Initrd.SHA256})
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(staged, metaName), record, 0o444); err != nil {
		return err
	}
	if err := syncDir(staged); err != nil {
		return err
	}
	if err := os.Rename(staged, s.setPath(name)); err != nil {
		if errors.Is(err, os.ErrExist) || errors.Is(err, syscall.ENOTEMPTY) {
			return machine.ErrBootSetExists
		}
		return err
	}
	published = true
	return errors.Join(syncDir(s.dir), syncDir(s.stagingDir()))
}

// writeVerified writes the content to a new read-only file, syncs it, and
// refuses it as a *machine.BootFileMismatch when its digest is not the one
// declared.
func writeVerified(ctx context.Context, path, file string, c machine.BootContent) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o444)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), &ctxReader{ctx: ctx, r: c.Content})
	if err == nil {
		err = f.Sync()
	}
	if err = errors.Join(err, f.Close()); err != nil {
		return fmt.Errorf("write the %s: %w", file, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != c.SHA256 {
		return &machine.BootFileMismatch{File: file, Got: got, Want: c.SHA256}
	}
	return nil
}

// ctxReader ends a copy when its context does.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// Get implements machine.BootSetStore.
func (s *Store) Get(ctx context.Context, name string) (machine.BootSet, error) {
	if err := ctx.Err(); err != nil {
		return machine.BootSet{}, err
	}
	if !validName.MatchString(name) {
		return machine.BootSet{}, machine.ErrBootSetAbsent
	}
	dir := s.setPath(name)
	raw, err := os.ReadFile(filepath.Join(dir, metaName))
	if errors.Is(err, os.ErrNotExist) {
		return machine.BootSet{}, machine.ErrBootSetAbsent
	}
	if err != nil {
		return machine.BootSet{}, err
	}
	var m meta
	if err := json.Unmarshal(raw, &m); err != nil {
		return machine.BootSet{}, fmt.Errorf("boot set %q: read the record: %w", name, err)
	}
	return machine.BootSet{
		Kernel: machine.KernelFile{Path: filepath.Join(dir, kernelName), SHA256: m.KernelSHA256},
		Initrd: machine.KernelFile{Path: filepath.Join(dir, initrdName), SHA256: m.InitrdSHA256},
	}, nil
}

// List implements machine.BootSetStore. A set removed between the listing of
// the directory and the read of its record is gone and left out.
func (s *Store) List(ctx context.Context) (machine.BootSets, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	sets := make(machine.BootSets, len(entries))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || !e.IsDir() {
			continue
		}
		set, err := s.Get(ctx, e.Name())
		if errors.Is(err, machine.ErrBootSetAbsent) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sets[e.Name()] = set
	}
	return sets, nil
}

// Remove implements machine.BootSetStore. The set leaves the boot sets by one
// rename, and its files are deleted after, so a crash in between leaves
// nothing a Get finds.
func (s *Store) Remove(ctx context.Context, name string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validName.MatchString(name) {
		return nil
	}
	retired, err := s.newStagingPath("removed-")
	if err != nil {
		return err
	}
	if err := os.Rename(s.setPath(name), retired); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if err := errors.Join(syncDir(s.dir), os.RemoveAll(retired)); err != nil {
		return err
	}
	return syncDir(s.stagingDir())
}
