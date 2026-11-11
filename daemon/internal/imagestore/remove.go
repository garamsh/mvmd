package imagestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// List implements machine.ImageStore: every listed image, ordered by the
// digest it was pulled by. An image's size is the allocated blocks of the
// blobs it reaches that the cache holds, so a blob two images share is in the
// size of each, and a manifest that cannot be read leaves the image with the
// size of what could be measured. An image published before the index
// recorded when it was stored has the zero time.
func (s *Store) List(ctx context.Context) ([]machine.ImageInfo, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	idx, err := s.readIndex()
	if err != nil {
		return nil, err
	}
	out := make([]machine.ImageInfo, 0, len(idx.Manifests))
	for _, d := range idx.Manifests {
		digest := machine.ImageDigest(d.Annotations[pulledByAnnotation])
		var created time.Time
		if text, ok := d.Annotations[createdAnnotation]; ok {
			created, err = time.Parse(time.RFC3339Nano, text)
			if err != nil {
				return nil, fmt.Errorf("image %s: read when it was stored: %w", digest, err)
			}
		}
		size, err := s.occupied(d)
		if err != nil {
			return nil, fmt.Errorf("image %s: measure it: %w", digest, err)
		}
		out = append(out, machine.ImageInfo{Digest: digest, SizeBytes: size, CreatedAt: created})
	}
	slices.SortFunc(out, func(a, b machine.ImageInfo) int { return strings.Compare(string(a.Digest), string(b.Digest)) })
	return out, nil
}

// occupied returns the allocated blocks of the blobs the listed image reaches.
// A blob that is absent occupies nothing.
func (s *Store) occupied(entry descriptor) (uint64, error) {
	digests := []string{entry.Digest}
	if _, reached, err := s.reaches(entry); err == nil {
		digests = reached
	}
	var total uint64
	for _, d := range digests {
		h, err := hexOf(d)
		if err != nil {
			return 0, err
		}
		blocks, err := allocated(s.blobPath(h))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return 0, err
		}
		total += blocks
	}
	return total, nil
}

// Remove implements machine.ImageStore. It unlists the image by one swap of
// index.json, then deletes the blobs the image reached that no remaining
// listed image reaches. It holds the mutex Publish holds, so a Publish lands
// wholly before it, and its blobs are then reached by the index Remove reads,
// or wholly after, and then moves in any blob Remove deleted. An error after
// the swap leaves the image unlisted and its blobs for the next start's sweep.
func (s *Store) Remove(ctx context.Context, digest machine.ImageDigest) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.swap.Lock()
	defer s.swap.Unlock()

	idx, err := s.readIndex()
	if err != nil {
		return err
	}
	entry, ok := idx.find(digest)
	if !ok {
		return nil
	}
	// What the image reached is read before the swap, but a manifest that
	// cannot be read does not keep the image listed: it is unlisted and its
	// blobs are left to the sweep.
	_, gone, goneErr := s.reaches(entry)

	idx.Manifests = slices.DeleteFunc(idx.Manifests, func(d descriptor) bool {
		return d.Annotations[pulledByAnnotation] == string(digest)
	})
	content, err := json.Marshal(idx)
	if err != nil {
		return err
	}
	fault := s.fault
	if fault == nil {
		fault = noFault
	}
	if err := replaceFile(s.indexPath(), content, fault); err != nil {
		return err
	}
	if err := syncDir(s.dir); err != nil {
		return err
	}
	if goneErr != nil {
		return fmt.Errorf("the image is unlisted and its blobs are left for the next start's sweep: %w", goneErr)
	}

	kept, err := s.reached(idx)
	if err != nil {
		return fmt.Errorf("the image is unlisted and its blobs are left for the next start's sweep: %w", err)
	}
	var errs []error
	for _, d := range gone {
		h, err := hexOf(d)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if !kept[h] {
			errs = append(errs, os.Remove(s.blobPath(h)))
		}
	}
	errs = append(errs, syncDir(s.blobsDir()))
	if err := errors.Join(withoutNotExist(errs)...); err != nil {
		return fmt.Errorf("the image is unlisted and some of its blobs are left for the next start's sweep: %w", err)
	}
	return nil
}

// reached returns the encoded digests of the blobs every image the index lists
// reaches. It fails when a listed manifest cannot be read.
func (s *Store) reached(idx index) (map[string]bool, error) {
	reached := make(map[string]bool)
	for _, entry := range idx.Manifests {
		_, digests, err := s.reaches(entry)
		if err != nil {
			return nil, fmt.Errorf("find what the listed image reaches: %w", err)
		}
		for _, d := range digests {
			h, err := hexOf(d)
			if err != nil {
				return nil, err
			}
			reached[h] = true
		}
	}
	return reached, nil
}
