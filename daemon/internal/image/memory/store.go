package memory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// Store is an in-memory image.Store. Its steps are "locate", "list", "remove", "stage", "lend",
// "reclaim", "put-layer", "seal", "record", "publish", "publish-sync",
// "discard" and "sweep"; "publish-sync" fails after the index was replaced, as
// the final sync of the adapter can. It holds the protocol of ADR 0007: a call
// out of order is an error, an image is listed only by a Publish that
// succeeded, and a Publish that fails has moved its blobs and left them
// unreferenced. It holds that of ADR 0024 too: a Seal of an output that is
// lent fails.
type Store struct {
	machinememory.Failures

	mu         sync.Mutex
	next       int
	listed     map[machine.ImageDigest]image.Record
	facts      map[machine.ImageDigest]machine.ImageInfo
	now        func() time.Time
	blobs      map[string]bool
	reached    map[machine.ImageDigest][]string
	staging    map[string]*stage
	publishing int
	overlapped bool
	onReclaim  func(image.Staging)
}

type stage struct {
	image  machine.ImageDigest
	lent   *machine.User
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
		facts:   make(map[machine.ImageDigest]machine.ImageInfo),
		now:     time.Now,
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
	run.Entrypoint = slices.Clone(run.Entrypoint)
	run.Cmd = slices.Clone(run.Cmd)
	run.Env = maps.Clone(run.Env)
	return machine.LocatedImage{Path: "/images/" + string(digest) + ".erofs", UUID: rec.UUID, Run: run}, nil
}

// UseClock makes Publish record the time the clock tells.
func (s *Store) UseClock(c machine.WallClock) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.now = c.Now
}

// List implements machine.ImageStore. Its step is "list". An image's size is
// the length its output was sealed at.
func (s *Store) List(context.Context) ([]machine.ImageInfo, error) {
	if err := s.Enter("list"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]machine.ImageInfo, 0, len(s.facts))
	for _, digest := range slices.Sorted(maps.Keys(s.facts)) {
		out = append(out, s.facts[digest])
	}
	return out, nil
}

// Remove implements machine.ImageStore. Its step is "remove". It unlists the
// image and deletes the blobs no remaining listed image reaches.
func (s *Store) Remove(_ context.Context, digest machine.ImageDigest) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	gone := s.reached[digest]
	delete(s.listed, digest)
	delete(s.facts, digest)
	delete(s.reached, digest)
	kept := make(map[string]bool)
	for _, names := range s.reached {
		for _, n := range names {
			kept[n] = true
		}
	}
	for _, n := range gone {
		if !kept[n] {
			delete(s.blobs, n)
		}
	}
	return nil
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

// Lend implements image.Store.
func (s *Store) Lend(_ context.Context, staging image.Staging, user machine.User) error {
	if err := s.Enter("lend"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.stageOf(staging)
	if err != nil {
		return err
	}
	st.lent = &user
	return nil
}

// Reclaim implements image.Store. It calls the function OnReclaim set, if any,
// before the output changes hands.
func (s *Store) Reclaim(_ context.Context, staging image.Staging) error {
	if err := s.Enter("reclaim"); err != nil {
		return err
	}
	s.mu.Lock()
	onReclaim := s.onReclaim
	s.mu.Unlock()
	if onReclaim != nil {
		onReclaim(staging)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	st, err := s.stageOf(staging)
	if err != nil {
		return err
	}
	st.lent = nil
	return nil
}

// OnReclaim makes the Store call f at each Reclaim, so a test sees what the
// world looks like when the output is taken back.
func (s *Store) OnReclaim(f func(image.Staging)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onReclaim = f
}

// Lent returns the user the output of the staging area is lent to, and
// whether it is lent.
func (s *Store) Lent(staging image.Staging) (machine.User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st, ok := s.staging[staging.ID]
	if !ok || st.lent == nil {
		return machine.User{}, false
	}
	return *st.lent, true
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
	if st.lent != nil {
		return "", fmt.Errorf("the output is still held by user %d", st.lent.UID)
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
	info := machine.ImageInfo{Digest: st.image, SizeBytes: st.length, CreatedAt: s.now()}
	s.mu.Unlock()

	// The index is replaced after the blobs are in place.
	if err := s.Enter("publish"); err != nil {
		return err
	}
	s.mu.Lock()
	s.listed[st.image] = record
	s.facts[st.image] = info
	s.reached[st.image] = names
	s.mu.Unlock()
	// The image is listed; an error from here on is a Publish that failed
	// with the image visible.
	return s.Enter("publish-sync")
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

// AsOldForm rewrites the listed image's record to the form written before the
// entrypoint, the command and the user were kept apart: one joined vector.
func (s *Store) AsOldForm(digest machine.ImageDigest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.listed[digest]
	rec.Run = machine.ImageRun{Entrypoint: rec.Run.Exec().Command, Env: rec.Run.Env, WorkingDir: rec.Run.WorkingDir, Joined: true}
	s.listed[digest] = rec
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
