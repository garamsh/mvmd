package imagestore

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ReplaceRun implements image.Store. The EROFS blob, the filesystem UUID and
// the fs-verity digest are the ones the listed image has; only the config and
// manifest blobs are new, and the index entry is swapped in one rename.
func (s *Store) ReplaceRun(ctx context.Context, digest machine.ImageDigest, run machine.ImageRun) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	fault := s.fault
	if fault == nil {
		fault = noFault
	}
	s.swap.Lock()
	defer s.swap.Unlock()
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	entry, ok := idx.find(digest)
	if !ok {
		return machine.ErrImageAbsent
	}
	var m manifest
	if err := s.readBlob(entry, &m); err != nil {
		return fmt.Errorf("image %s: read the manifest: %w", digest, err)
	}
	var c config
	if err := s.readBlob(m.Config, &c); err != nil {
		return fmt.Errorf("image %s: read the config: %w", digest, err)
	}
	_ = run
	configBytes, err := json.Marshal(c)
	if err != nil {
		return err
	}
	m.Config = descriptor{MediaType: m.Config.MediaType, Digest: "sha256:" + sha256Hex(configBytes), Size: int64(len(configBytes))}
	manifestBytes, err := json.Marshal(m)
	if err != nil {
		return err
	}
	for _, b := range []struct {
		name  string
		bytes []byte
	}{{"config", configBytes}, {"manifest", manifestBytes}} {
		if err := fault("move-blob"); err != nil {
			return err
		}
		if err := s.putBlob(b.name, b.bytes); err != nil {
			return err
		}
	}
	if err := fault("sync-blobs"); err != nil {
		return err
	}
	if err := syncDir(s.blobsDir()); err != nil {
		return err
	}
	entry.Digest = "sha256:" + sha256Hex(manifestBytes)
	entry.Size = int64(len(manifestBytes))
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

// putBlob writes the content as a blob named by its digest, and keeps the blob
// already there when there is one.
func (s *Store) putBlob(name string, content []byte) error {
	hex := sha256Hex(content)
	if _, err := os.Stat(s.blobPath(hex)); err == nil {
		return nil
	}
	tmp := filepath.Join(s.stagingDir(), "replace-"+name+"-"+hex)
	if err := os.Remove(tmp); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := writeFileSync(tmp, content); err != nil {
		return err
	}
	return s.moveBlob(tmp, hex)
}
