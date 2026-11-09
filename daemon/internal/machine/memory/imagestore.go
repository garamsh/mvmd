package memory

import (
	"context"
	"maps"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ImageStore is an in-memory machine.ImageStore. Its step is "locate".
type ImageStore struct {
	Failures

	mu     sync.Mutex
	images map[machine.ImageDigest]machine.LocatedImage
}

// NewImageStore returns an ImageStore that holds no image.
func NewImageStore() *ImageStore {
	return &ImageStore{images: make(map[machine.ImageDigest]machine.LocatedImage)}
}

// Provide makes the node hold an image, with a filesystem UUID and a run
// configuration of its own.
func (s *ImageStore) Provide(image machine.ImageDigest) {
	s.ProvideWith(image, machine.LocatedImage{
		UUID: "erofs-uuid-of-" + string(image),
		Run: machine.ExecSpec{
			Command:    []string{"/bin/run", string(image)},
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
	located.Run.Command = slices.Clone(located.Run.Command)
	located.Run.Env = maps.Clone(located.Run.Env)
	return located, nil
}
