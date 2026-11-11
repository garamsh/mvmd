package imagestore

import (
	"os"
	"path/filepath"
	"time"
)

// SetFault makes the Store ask fault before each step of Publish, and fail the
// step with what it returns.
func (s *Store) SetFault(fault func(step string) error) { s.fault = fault }

// SetClock makes Publish record the time now returns.
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// OpenWithoutVerity opens the image cache in dir as New does, without the
// probe for fs-verity, for a test of what List reads from files it wrote by
// hand. Such a cache cannot Seal or Locate.
func OpenWithoutVerity(dir string) (*Store, error) {
	s := &Store{dir: dir, stages: make(map[string]*stage)}
	for _, d := range []string{s.blobsDir(), s.stagingDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	if err := s.createIfAbsent(filepath.Join(dir, "oci-layout"), []byte(layoutVersionContent)); err != nil {
		return nil, err
	}
	empty := []byte(`{"schemaVersion":2,"mediaType":"` + ociIndexType + `","manifests":[]}`)
	return s, s.createIfAbsent(s.indexPath(), empty)
}
