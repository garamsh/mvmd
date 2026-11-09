package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// Store is an in-memory image.Store. Its steps are "locate", "stage",
// "put-layer", "seal", "record", "publish", "discard" and "sweep". It holds
// the protocol of ADR 0007: a call out of order is an error, an image is
// listed only by a Publish that succeeded, and a Publish that fails has moved
// its blobs and left them unreferenced.
type Store struct {
	machinememory.Failures

	mu         sync.Mutex
	next       int
	listed     map[machine.ImageDigest]image.Record
	blobs      map[string]bool
	reached    map[machine.ImageDigest][]string
	staging    map[string]*stage
	publishing int
	overlapped bool
}

type stage struct {
	image  machine.ImageDigest
	layers map[string][]byte
	sealed bool
	length uint64
	verity string
	record *image.Record
}

// NewStore returns a Store that lists no image.
func NewStore() *Store {
	return &Store{
		listed:  make(map[machine.ImageDigest]image.Record),
		blobs:   make(map[string]bool),
		reached: make(map[machine.ImageDigest][]string),
		staging: make(map[string]*stage),
	}
}

// Locate implements machine.ImageStore.
func (s *Store) Locate(_ context.Context, digest machine.ImageDigest) (machine.LocatedImage, error) {
	if err := s.Enter("locate"); err != nil {
		return machine.LocatedImage{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.listed[digest]
	if !ok {
		return machine.LocatedImage{}, machine.ErrImageAbsent
	}
	run := rec.Run
	run.Command = slices.Clone(run.Command)
	run.Env = maps.Clone(run.Env)
	return machine.LocatedImage{Path: "/images/" + string(digest) + ".erofs", UUID: rec.UUID, Run: run}, nil
}

// Stage implements image.Store.
func (s *Store) Stage(_ context.Context, digest machine.ImageDigest, _ uint64) (image.Staging, error) {
	if err := s.Enter("stage"); err != nil {
		return image.Staging{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.open(digest), nil
}

// open creates a staging area. The caller holds mu.
func (s *Store) open(digest machine.ImageDigest) image.Staging {
	s.next++
	id := fmt.Sprintf("staging-%d", s.next)
	s.staging[id] = &stage{image: digest, layers: make(map[string][]byte)}
	return image.Staging{ID: id, Output: "/staging/" + id + "/output"}
}

// PutLayer implements image.Store.
func (s *Store) PutLayer(_ context.Context, staging image.Staging, index int, content io.Reader) (string, error) {
	if err := s.Enter("put-layer"); err != nil {
		return "", err
	}
	bytes, err := io.ReadAll(content)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.stageOf(staging)
	if err != nil {
		return "", err
	}
	path := fmt.Sprintf("/staging/%s/layer-%d", staging.ID, index)
	st.layers[path] = bytes
	return path, nil
}

// Seal implements image.Store. The digest it returns is a function of the
// image and the length.
func (s *Store) Seal(_ context.Context, staging image.Staging, length uint64) (string, error) {
	if err := s.Enter("seal"); err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.stageOf(staging)
	if err != nil {
		return "", err
	}
	st.sealed, st.length = true, length
	st.verity = digestOf(fmt.Appendf(nil, "%s|%d", st.image, length))
	return st.verity, nil
}

// Record implements image.Store.
func (s *Store) Record(_ context.Context, staging image.Staging, record image.Record) error {
	if err := s.Enter("record"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.stageOf(staging)
	if err != nil {
		return err
	}
	if !st.sealed {
		return errors.New("record before seal")
	}
	if record.Verity != st.verity {
		return fmt.Errorf("record carries the digest %q, but the output measured %q", record.Verity, st.verity)
	}
	st.record = &record
	return nil
}

// Publish implements image.Store. It records that it ran while another
// Publish did, which the contract leaves to the caller to prevent;
// PublishOverlapped reports it.
func (s *Store) Publish(_ context.Context, staging image.Staging) error {
	s.mu.Lock()
	s.publishing++
	if s.publishing > 1 {
		s.overlapped = true
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.publishing--
		s.mu.Unlock()
	}()

	s.mu.Lock()
	st, err := s.stageOf(staging)
	if err == nil && st.record == nil {
		err = errors.New("publish before record")
	}
	if err != nil {
		s.mu.Unlock()
		return err
	}
	names := []string{string(st.image) + "/erofs", string(st.image) + "/config", string(st.image) + "/manifest"}
	for _, n := range names {
		s.blobs[n] = true
	}
	record := *st.record
	s.mu.Unlock()

	// The index is replaced after the blobs are in place.
	if err := s.Enter("publish"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listed[st.image] = record
	s.reached[st.image] = names
	return nil
}

// Discard implements image.Store.
func (s *Store) Discard(ctx context.Context, staging image.Staging) error {
	if err := s.Enter("discard"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.staging, staging.ID)
	return nil
}

// Sweep implements image.Store.
func (s *Store) Sweep(context.Context) error {
	if err := s.Enter("sweep"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	clear(s.staging)
	reached := make(map[string]bool)
	for _, names := range s.reached {
		for _, n := range names {
			reached[n] = true
		}
	}
	for n := range s.blobs {
		if !reached[n] {
			delete(s.blobs, n)
		}
	}
	return nil
}

// stageOf returns the staging area. The caller holds mu.
func (s *Store) stageOf(staging image.Staging) (*stage, error) {
	st, ok := s.staging[staging.ID]
	if !ok {
		return nil, fmt.Errorf("no staging area %q", staging.ID)
	}
	return st, nil
}

// Listed returns the images the cache lists, ordered.
func (s *Store) Listed() []machine.ImageDigest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Sorted(maps.Keys(s.listed))
}

// Staging returns how many staging areas exist.
func (s *Store) Staging() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.staging)
}

// Unreferenced returns how many blobs no listed image reaches.
func (s *Store) Unreferenced() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	reached := make(map[string]bool)
	for _, names := range s.reached {
		for _, n := range names {
			reached[n] = true
		}
	}
	n := 0
	for name := range s.blobs {
		if !reached[name] {
			n++
		}
	}
	return n
}

// Layer returns the bytes stored at the path of a staged layer.
func (s *Store) Layer(path string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, st := range s.staging {
		if b, ok := st.layers[path]; ok {
			return slices.Clone(b), true
		}
	}
	return nil, false
}

// RecordOf returns what the cache lists the image with.
func (s *Store) RecordOf(digest machine.ImageDigest) (image.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.listed[digest]
	return rec, ok
}

// Leave makes the store hold what a pull that crashed leaves: a staging area
// and a blob no listed image reaches.
func (s *Store) Leave(digest machine.ImageDigest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.open(digest)
	s.blobs[string(digest)+"/erofs"] = true
}

// PublishOverlapped reports whether two calls of Publish ever ran at once.
func (s *Store) PublishOverlapped() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.overlapped
}
