// Package imagestore is mvmd's Image Store: the node image cache as one OCI
// image layout on disk, with fs-verity on every EROFS file. A pull works in a
// staging area outside the layout and becomes visible by one swap of
// index.json (ADR 0007); Locate measures the file against the digest recorded
// when it was sealed before it answers (ADR 0008).
package imagestore

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ image.Store = (*Store)(nil)

// Store is the image cache in one directory. Stage, PutLayer, Seal, Record
// and Discard are safe to call for different pulls at once; Publish is
// serialized by the caller, and Sweep runs before any other call.
type Store struct {
	dir string

	// fault is the seam of the tests: it is asked before each step of Publish
	// and may fail it. It is nil in production.
	fault func(step string) error

	// swap is held by Publish from its first blob move to the swap of
	// index.json, and by Remove throughout: both read the index, rewrite it
	// and move or delete the blobs it lists.
	swap sync.Mutex

	mu     sync.Mutex
	stages map[string]*stage
}

// stage is what the Store holds of one staging area between its calls.
type stage struct {
	image  machine.ImageDigest
	sealed *sealed
	record *recorded
}

type sealed struct {
	hex    string
	size   int64
	verity string
}

type recorded struct {
	configHex    string
	manifestHex  string
	manifestSize int64
}

// New opens the image cache in dir, creating the layout when dir holds none.
// It fails when the file system under dir cannot hold fs-verity files, which
// the cache cannot do without (ADR 0008).
func New(dir string) (*Store, error) {
	s := &Store{dir: dir, stages: make(map[string]*stage)}
	for _, d := range []string{s.blobsDir(), s.stagingDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, fmt.Errorf("create the image cache: %w", err)
		}
	}
	if err := s.createIfAbsent(filepath.Join(dir, "oci-layout"), []byte(layoutVersionContent)); err != nil {
		return nil, fmt.Errorf("create the image cache: %w", err)
	}
	empty := []byte(`{"schemaVersion":2,"mediaType":"` + ociIndexType + `","manifests":[]}`)
	if err := s.createIfAbsent(s.indexPath(), empty); err != nil {
		return nil, fmt.Errorf("create the image cache: %w", err)
	}
	if err := s.repairImageModes(); err != nil {
		return nil, fmt.Errorf("set the mode of the cache's images: %w", err)
	}
	if err := s.probeVerity(); err != nil {
		return nil, fmt.Errorf("the image cache at %s cannot hold fs-verity files, so mvmd will not start; its state directory has to be on ext4 or f2fs with the verity feature, or on btrfs: %w", dir, err)
	}
	return s, nil
}

// repairImageModes gives every listed image the mode of a published one, so a
// cache published before the mode was set starts Machines too. An image whose
// manifest or file is absent is left for Locate and Sweep to report.
func (s *Store) repairImageModes() error {
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	for _, entry := range idx.Manifests {
		var m manifest
		if err := s.readBlob(entry, &m); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("read the manifest %s: %w", entry.Digest, err)
		}
		for _, l := range m.Layers {
			h, err := hexOf(l.Digest)
			if err != nil {
				return err
			}
			if err := os.Chmod(s.blobPath(h), imageMode); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

// createIfAbsent writes the file durably when there is none.
func (s *Store) createIfAbsent(path string, content []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := replaceFile(path, content, noFault); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// probeVerity enables fs-verity on a file of the cache's own file system and
// removes it.
func (s *Store) probeVerity() error {
	probe, err := os.CreateTemp(s.stagingDir(), "verity-probe-")
	if err != nil {
		return err
	}
	defer os.Remove(probe.Name())
	_, werr := probe.Write(make([]byte, verityBlockSize))
	serr := probe.Sync()
	if err := errors.Join(werr, serr, probe.Close()); err != nil {
		return err
	}
	return enableVerity(probe.Name())
}

// Locate implements machine.ImageStore. The file's digest is measured on
// every call, so the file of an image whose bytes or verity state differ from
// what Seal recorded is never returned.
func (s *Store) Locate(ctx context.Context, digest machine.ImageDigest) (machine.LocatedImage, error) {
	if err := ctx.Err(); err != nil {
		return machine.LocatedImage{}, err
	}
	idx, err := s.readIndex()
	if err != nil {
		return machine.LocatedImage{}, err
	}
	entry, ok := idx.find(digest)
	if !ok {
		return machine.LocatedImage{}, machine.ErrImageAbsent
	}
	var m manifest
	if err := s.readBlob(entry, &m); err != nil {
		return machine.LocatedImage{}, fmt.Errorf("image %s: read the manifest: %w", digest, err)
	}
	if m.ArtifactType != machineImageType || len(m.Layers) != 1 || m.Layers[0].MediaType != machineImageFilesys {
		return machine.LocatedImage{}, fmt.Errorf("image %s: the manifest %s is not a MachineImage", digest, entry.Digest)
	}
	var c config
	if err := s.readBlob(m.Config, &c); err != nil {
		return machine.LocatedImage{}, fmt.Errorf("image %s: read the config: %w", digest, err)
	}
	layer := m.Layers[0]
	h, err := hexOf(layer.Digest)
	if err != nil {
		return machine.LocatedImage{}, fmt.Errorf("image %s: %w", digest, err)
	}
	path := s.blobPath(h)
	want := layer.Annotations[verityAnnotation]
	got, err := measureVerity(path)
	if err != nil {
		return machine.LocatedImage{}, fmt.Errorf("image %s: the file %s fails the fs-verity check: %w", digest, path, err)
	}
	if got != want {
		return machine.LocatedImage{}, fmt.Errorf("image %s: the file %s has the fs-verity digest %s, recorded as %s", digest, path, got, want)
	}
	return machine.LocatedImage{Path: path, UUID: c.UUID, Run: c.Run.exec()}, nil
}
