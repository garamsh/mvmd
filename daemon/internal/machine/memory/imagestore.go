package memory

import (
	"context"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ImageStore is an in-memory machine.ImageStore. Its steps are "locate",
// "list" and "remove".
type ImageStore struct {
	Failures

	mu     sync.Mutex
	images map[machine.ImageDigest]machine.LocatedImage
	facts  map[machine.ImageDigest]machine.ImageInfo
}

// NewImageStore returns an ImageStore that holds no image.
func NewImageStore() *ImageStore {
	return &ImageStore{images: make(map[machine.ImageDigest]machine.LocatedImage), facts: make(map[machine.ImageDigest]machine.ImageInfo)}
}

// Provide makes the node hold an image, with a filesystem UUID and a run
// configuration of its own.
func (s *ImageStore) Provide(image machine.ImageDigest) {
	s.ProvideWith(image, machine.LocatedImage{
		UUID: "erofs-uuid-of-" + string(image),
		Run: machine.ImageRun{
			Entrypoint: []string{"/bin/run", string(image)},
			Env:        map[string]string{"IMAGE": string(image)},
			WorkingDir: "/srv",
		},
	})
}

// ProvideWith makes the node hold an image as given. The path is the
// substitute's own, so a Path in located is ignored.
func (s *ImageStore) ProvideWith(image machine.ImageDigest, located machine.LocatedImage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	located.Path = "/images/" + string(image) + ".erofs"
	s.images[image] = located
}

// SetFacts sets the size and the storing time List reports for an image the
// node holds. An image Provide made reports a size of zero and no time.
func (s *ImageStore) SetFacts(image machine.ImageDigest, sizeBytes uint64, createdAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.facts[image] = machine.ImageInfo{Digest: image, SizeBytes: sizeBytes, CreatedAt: createdAt}
}

// Locate implements machine.ImageStore.
func (s *ImageStore) Locate(_ context.Context, image machine.ImageDigest) (machine.LocatedImage, error) {
	if err := s.Enter("locate"); err != nil {
		return machine.LocatedImage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	located, ok := s.images[image]
	if !ok {
		return machine.LocatedImage{}, machine.ErrImageAbsent
	}
	// A copy, so a caller cannot change what the store holds.
	located.Run.Entrypoint = slices.Clone(located.Run.Entrypoint)
	located.Run.Cmd = slices.Clone(located.Run.Cmd)
	located.Run.Env = maps.Clone(located.Run.Env)
	return located, nil
}

// List implements machine.ImageStore.
func (s *ImageStore) List(context.Context) ([]machine.ImageInfo, error) {
	if err := s.Enter("list"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]machine.ImageInfo, 0, len(s.images))
	for _, digest := range slices.Sorted(maps.Keys(s.images)) {
		info := s.facts[digest]
		info.Digest = digest
		out = append(out, info)
	}
	return out, nil
}

// Remove implements machine.ImageStore.
func (s *ImageStore) Remove(_ context.Context, image machine.ImageDigest) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.images, image)
	delete(s.facts, image)
	return nil
}
