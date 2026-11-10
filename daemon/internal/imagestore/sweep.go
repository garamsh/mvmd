package imagestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
)

// Sweep implements image.Store. It removes every staging area, the temporary
// file of an interrupted index swap, and every blob no listed manifest
// reaches. It fails without removing a blob when a listed manifest cannot be
// read, since what that image reaches is then unknown.
func (s *Store) Sweep(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	reached, err := s.reached(idx)
	if err != nil {
		return err
	}

	staged, err := os.ReadDir(s.stagingDir())
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range staged {
		errs = append(errs, os.RemoveAll(filepath.Join(s.stagingDir(), e.Name())))
	}
	errs = append(errs, os.Remove(s.indexPath()+".tmp"))
	blobs, err := os.ReadDir(s.blobsDir())
	if err != nil {
		return err
	}
	for _, e := range blobs {
		if !reached[e.Name()] {
			errs = append(errs, os.RemoveAll(filepath.Join(s.blobsDir(), e.Name())))
		}
	}
	errs = append(errs, syncDir(s.blobsDir()))

	s.mu.Lock()
	clear(s.stages)
	s.mu.Unlock()
	return errors.Join(withoutNotExist(errs)...)
}

func withoutNotExist(errs []error) []error {
	var kept []error
	for _, err := range errs {
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			kept = append(kept, err)
		}
	}
	return kept
}
