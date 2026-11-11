package imagestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/image"
)

// Publish implements image.Store. A failure before the rename of index.json
// leaves the cache listing what it listed, and the blobs moved so far
// unreferenced for the sweep.
func (s *Store) Publish(ctx context.Context, staging image.Staging) error {
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
	if st.record == nil {
		return errors.New("publish before record")
	}
	fault := s.fault
	if fault == nil {
		fault = noFault
	}

	// The image gets its mode while the daemon owns the output, which Seal
	// checked, and before the rename that makes it a blob, so no blob of the
	// layout is an image with another mode.
	if err := s.setImageMode(ctx, staging); err != nil {
		return fmt.Errorf("set the mode of the image: %w", err)
	}

	// A Remove lands wholly before the first blob moves or wholly after the
	// swap, so it never deletes a blob this Publish found in place.
	s.swap.Lock()
	defer s.swap.Unlock()
	blobs := []struct{ from, hex string }{
		{"output", st.sealed.hex},
		{"config", st.record.configHex},
		{"manifest", st.record.manifestHex},
	}
	for _, b := range blobs {
		if err := fault("move-blob"); err != nil {
			return err
		}
		if err := s.moveBlob(filepath.Join(dir, b.from), b.hex); err != nil {
			return err
		}
	}
	if err := fault("sync-blobs"); err != nil {
		return err
	}
	for _, b := range blobs {
		if err := syncFile(s.blobPath(b.hex)); err != nil {
			return err
		}
	}
	if err := syncDir(s.blobsDir()); err != nil {
		return err
	}

	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	entry := descriptor{
		MediaType: ociManifestType,
		Digest:    "sha256:" + st.record.manifestHex,
		Size:      st.record.manifestSize,
		Annotations: map[string]string{
			pulledByAnnotation: string(st.image),
		},
	}
	idx.Manifests = withEntry(idx.Manifests, entry)
	content, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	if err := replaceFile(s.indexPath(), content, fault); err != nil {
		return err
	}
	if err := fault("sync-dir"); err != nil {
		return err
	}
	return syncDir(s.dir)
}

// setImageMode gives the staged output the mode of a published image.
func (s *Store) setImageMode(ctx context.Context, staging image.Staging) error {
	f, err := s.openOutput(ctx, staging)
	if err != nil {
		return err
	}
	return errors.Join(f.Chmod(imageMode), f.Close())
}

// moveBlob renames the staged file to the blob's name, and keeps the blob
// already there when there is one.
func (s *Store) moveBlob(from, hex string) error {
	to := s.blobPath(hex)
	if _, err := os.Stat(to); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Rename(from, to); err != nil {
		return fmt.Errorf("move %s into the layout: %w", filepath.Base(from), err)
	}
	return nil
}

// withEntry returns the entries with the one for the image replacing any
// earlier entry for it.
func withEntry(entries []descriptor, entry descriptor) []descriptor {
	out := make([]descriptor, 0, len(entries)+1)
	for _, e := range entries {
		if e.Annotations[pulledByAnnotation] != entry.Annotations[pulledByAnnotation] {
			out = append(out, e)
		}
	}
	return append(out, entry)
}
