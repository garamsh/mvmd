package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// BootSetStore is an in-memory machine.BootSetStore: it checks each file's
// digest as the real store does and keeps the digests, not the bytes.
type BootSetStore struct {
	mu   sync.Mutex
	sets machine.BootSets
}

var _ machine.BootSetStore = (*BootSetStore)(nil)

// NewBootSetStore returns an empty store.
func NewBootSetStore() *BootSetStore {
	return &BootSetStore{sets: machine.BootSets{}}
}

// Add implements machine.BootSetStore.
func (b *BootSetStore) Add(_ context.Context, name string, upload machine.BootUpload) error {
	for _, f := range []struct {
		file    string
		content machine.BootContent
	}{{"kernel", upload.Kernel}, {"initrd", upload.Initrd}} {
		h := sha256.New()
		if _, err := io.Copy(h, f.content.Content); err != nil {
			return err
		}
		if got := hex.EncodeToString(h.Sum(nil)); got != f.content.SHA256 {
			return &machine.BootFileMismatch{File: f.file, Got: got, Want: f.content.SHA256}
		}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if _, ok := b.sets[name]; ok {
		return machine.ErrBootSetExists
	}
	b.sets[name] = machine.BootSet{
		Kernel: machine.KernelFile{Path: "/memory/boot-sets/" + name + "/kernel", SHA256: upload.Kernel.SHA256},
		Initrd: machine.KernelFile{Path: "/memory/boot-sets/" + name + "/initrd", SHA256: upload.Initrd.SHA256},
	}
	return nil
}

// Get implements machine.BootSetStore.
func (b *BootSetStore) Get(_ context.Context, name string) (machine.BootSet, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	set, ok := b.sets[name]
	if !ok {
		return machine.BootSet{}, machine.ErrBootSetAbsent
	}
	return set, nil
}

// List implements machine.BootSetStore.
func (b *BootSetStore) List(context.Context) (machine.BootSets, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make(machine.BootSets, len(b.sets))
	for n, s := range b.sets {
		out[n] = s
	}
	return out, nil
}

// Remove implements machine.BootSetStore.
func (b *BootSetStore) Remove(_ context.Context, name string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.sets, name)
	return nil
}
