package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ImageStore is an in-memory machine.ImageStore. Its step is "locate".
type ImageStore struct {
	Failures

	mu     sync.Mutex
	images map[machine.ImageDigest]bool
}

// NewImageStore returns an ImageStore that holds no image.
func NewImageStore() *ImageStore {
	return &ImageStore{images: make(map[machine.ImageDigest]bool)}
}

// Provide makes the node hold an image.
func (s *ImageStore) Provide(image machine.ImageDigest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images[image] = true
}

// Locate implements machine.ImageStore.
func (s *ImageStore) Locate(_ context.Context, image machine.ImageDigest) (string, error) {
	if err := s.enter("locate"); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.images[image] {
		return "", machine.ErrImageAbsent
	}
	return "/images/" + string(image) + ".erofs", nil
}
