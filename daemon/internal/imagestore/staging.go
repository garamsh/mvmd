package imagestore

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
	"syscall"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var stagingIDForm = regexp.MustCompile(`^[0-9a-f]{32}$`)

// stagePath is the directory of a staging area. The identifier is checked, so
// it is never a path that leaves staging.
func (s *Store) stagePath(id string) (string, error) {
	if !stagingIDForm.MatchString(id) {
		return "", fmt.Errorf("%q is not a staging area of this store", id)
	}
	return filepath.Join(s.stagingDir(), id), nil
}

// Stage implements image.Store.
func (s *Store) Stage(ctx context.Context, digest machine.ImageDigest, outputBytes uint64) (image.Staging, error) {
	if err := ctx.Err(); err != nil {
		return image.Staging{}, err
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return image.Staging{}, err
	}
	id := hex.EncodeToString(raw)
	dir := filepath.Join(s.stagingDir(), id)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return image.Staging{}, err
	}
	output := filepath.Join(dir, "output")
	if err := createSparse(output, outputBytes); err != nil {
		return image.Staging{}, errors.Join(err, os.RemoveAll(dir))
	}
	s.mu.Lock()
	s.stages[id] = &stage{image: digest}
	s.mu.Unlock()
	return image.Staging{ID: id, Output: output}, nil
}

func createSparse(path string, size uint64) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, outputMode)
	if err != nil {
		return err
	}
	return errors.Join(f.Truncate(int64(size)), f.Close())
}

// outputMode is the mode the output is created with and Reclaim restores: the
// owner's alone, whoever the owner is (ADR 0023).
const outputMode = 0o600

// Lend implements image.Store. The output keeps its mode; only its owner
// changes, so the user gains what the owner's bits grant and nobody else
// gains anything.
func (s *Store) Lend(ctx context.Context, staging image.Staging, user machine.User) error {
	f, err := s.openOutput(ctx, staging)
	if err != nil {
		return err
	}
	return errors.Join(f.Chown(int(user.UID), int(user.GID)), f.Close())
}

// Reclaim implements image.Store. It also sets the mode again, since the user
// who held the file could change it.
func (s *Store) Reclaim(ctx context.Context, staging image.Staging) error {
	f, err := s.openOutput(ctx, staging)
	if err != nil {
		return err
	}
	if err := f.Chown(os.Geteuid(), os.Getegid()); err != nil {
		return errors.Join(err, f.Close())
	}
	return f.Close()
}

// openOutput opens the staging area's output without following a link, so
// what changes hands is the file the area made.
func (s *Store) openOutput(ctx context.Context, staging image.Staging) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	dir, err := s.stagePath(staging.ID)
	if err != nil {
		return nil, err
	}
	if _, err := s.stageOf(staging); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dir, "output"), os.O_RDONLY, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	if !info.Mode().IsRegular() {
		return nil, errors.Join(fmt.Errorf("the output of staging area %q is not a regular file", staging.ID), f.Close())
	}
	return f, nil
}

// requireDaemonsOutput fails when the output belongs to another user, which is
// how a Seal knows a Lend was not reclaimed.
func requireDaemonsOutput(output string) error {
	info, err := os.Lstat(output)
	if err != nil {
		return err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.New("the output's owner cannot be read")
	}
	if int(st.Uid) != os.Geteuid() {
		return fmt.Errorf("the output is still held by user %d", st.Uid)
	}
	return nil
}

// PutLayer implements image.Store.
func (s *Store) PutLayer(ctx context.Context, staging image.Staging, index int, content io.Reader) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := s.stagePath(staging.ID)
	if err != nil {
		return "", err
	}
	if _, err := s.stageOf(staging); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("layer-%d", index))
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, content); err != nil {
		return "", errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return "", errors.Join(err, f.Close())
	}
	return path, f.Close()
}

// Seal implements image.Store.
func (s *Store) Seal(ctx context.Context, staging image.Staging, length uint64) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	dir, err := s.stagePath(staging.ID)
	if err != nil {
		return "", err
	}
	st, err := s.stageOf(staging)
	if err != nil {
		return "", err
	}
	output := filepath.Join(dir, "output")
	size, sum, err := truncateAndHash(output, length)
	if err != nil {
		return "", err
	}
	if err := enableVerity(output); err != nil {
		return "", err
	}
	verity, err := measureVerity(output)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	st.sealed = &sealed{hex: sum, size: size, verity: verity}
	s.mu.Unlock()
	return verity, nil
}

// truncateAndHash cuts the file to length, flushes it and returns its size
// and SHA-256. The file is closed when it returns, as enabling fs-verity
// needs.
func truncateAndHash(path string, length uint64) (int64, string, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return 0, "", err
	}
	if err := f.Truncate(int64(length)); err != nil {
		return 0, "", errors.Join(err, f.Close())
	}
	if err := f.Sync(); err != nil {
		return 0, "", errors.Join(err, f.Close())
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err := errors.Join(err, f.Close()); err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// Record implements image.Store.
func (s *Store) Record(ctx context.Context, staging image.Staging, record image.Record) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.stagePath(staging.ID)
	if err != nil {
		return err
	}
	st, err := s.stageOf(staging)
	if err != nil {
		return err
	}
	if st.sealed == nil {
		return errors.New("record before seal")
	}
	if record.Verity != st.sealed.verity {
		return fmt.Errorf("record carries the digest %q, but the output measured %q", record.Verity, st.sealed.verity)
	}
	if record.Image != st.image {
		return fmt.Errorf("record is for image %q, staged for %q", record.Image, st.image)
	}
	configBytes, err := json.Marshal(config{
		Architecture: record.Architecture,
		Converter:    record.Converter,
		UUID:         record.UUID,
		Run:          toRunSpec(record.Run),
		Kernel:       []string{},
	})
	if err != nil {
		return err
	}
	configHex := sha256Hex(configBytes)
	manifestBytes, err := json.Marshal(manifest{
		SchemaVersion: 2,
		MediaType:     ociManifestType,
		ArtifactType:  machineImageType,
		Config:        descriptor{MediaType: machineImageConfig, Digest: "sha256:" + configHex, Size: int64(len(configBytes))},
		Layers: []descriptor{{
			MediaType:   machineImageFilesys,
			Digest:      "sha256:" + st.sealed.hex,
			Size:        st.sealed.size,
			Annotations: map[string]string{verityAnnotation: st.sealed.verity},
		}},
	})
	if err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, "config"), configBytes); err != nil {
		return err
	}
	if err := writeFileSync(filepath.Join(dir, "manifest"), manifestBytes); err != nil {
		return err
	}
	s.mu.Lock()
	st.record = &recorded{configHex: configHex, manifestHex: sha256Hex(manifestBytes), manifestSize: int64(len(manifestBytes))}
	s.mu.Unlock()
	return nil
}

// Discard implements image.Store. It removes the area whether or not this
// Store opened it, so one left by an earlier run goes the same way.
func (s *Store) Discard(ctx context.Context, staging image.Staging) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	dir, err := s.stagePath(staging.ID)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.stages, staging.ID)
	s.mu.Unlock()
	return nil
}

func (s *Store) stageOf(staging image.Staging) (*stage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.stages[staging.ID]
	if !ok {
		return nil, fmt.Errorf("no staging area %q", staging.ID)
	}
	return st, nil
}
