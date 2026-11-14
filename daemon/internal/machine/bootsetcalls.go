package machine

import (
	"context"
	"errors"
	"regexp"
)

// BootSetOrigin is how a boot set came to be offered.
type BootSetOrigin string

const (
	// BootSetConfigured is a set the node's configuration names.
	BootSetConfigured BootSetOrigin = "configured"
	// BootSetAdded is a set a client added (ADR 0063).
	BootSetAdded BootSetOrigin = "added"
)

// BootSetInfo is a boot set the node offers and how it came to be offered.
type BootSetInfo struct {
	Name   string
	Set    BootSet
	Origin BootSetOrigin
}

var (
	bootSetName   = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	bootSetDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func (b BootSet) sameDigests(o BootSet) bool {
	return b.Kernel.SHA256 == o.Kernel.SHA256 && b.Initrd.SHA256 == o.Initrd.SHA256
}

// resolveBootSet finds the set the name offers, a configured one before an
// added one, and reports its origin. A name that is not offered is
// ErrBootSetAbsent.
func (s *service) resolveBootSet(ctx context.Context, name string) (BootSet, BootSetOrigin, error) {
	if set, ok := s.bootSets[name]; ok {
		return set, BootSetConfigured, nil
	}
	if s.addedSets == nil {
		return BootSet{}, "", ErrBootSetAbsent
	}
	set, err := s.addedSets.Get(ctx, name)
	return set, BootSetAdded, err
}

func (s *service) AddBootSet(ctx context.Context, name string, upload BootUpload) (BootSetInfo, error) {
	declared := BootSet{Kernel: KernelFile{SHA256: upload.Kernel.SHA256}, Initrd: KernelFile{SHA256: upload.Initrd.SHA256}}
	switch {
	case !bootSetName.MatchString(name):
		return BootSetInfo{}, fail(ErrInvalid, "boot set name %q is not 1 to 63 characters of a-z, 0-9 and \"-\"", name)
	case !bootSetDigest.MatchString(declared.Kernel.SHA256):
		return BootSetInfo{}, fail(ErrInvalid, "the kernel digest %q is not 64 lowercase hexadecimal digits", declared.Kernel.SHA256)
	case !bootSetDigest.MatchString(declared.Initrd.SHA256):
		return BootSetInfo{}, fail(ErrInvalid, "the initrd digest %q is not 64 lowercase hexadecimal digits", declared.Initrd.SHA256)
	}
	if s.addedSets == nil {
		return BootSetInfo{}, fail(ErrUnsupported, "the node keeps no boot set a client adds")
	}

	// The caller's own context bounds the upload: a stream that breaks ends
	// the call with nothing published.
	held, err := s.offeredAs(ctx, name, declared)
	if err != nil || held != nil {
		return derefInfo(held), err
	}
	switch err := s.addedSets.Add(ctx, name, upload); {
	case err == nil:
	case errors.Is(err, ErrBootSetExists):
		// Another add of the name published first.
		held, err := s.offeredAs(ctx, name, declared)
		if err != nil {
			return BootSetInfo{}, err
		}
		return derefInfo(held), nil
	default:
		var mismatch *BootFileMismatch
		if errors.As(err, &mismatch) {
			return BootSetInfo{}, fail(ErrInvalid, "boot set %q: %v", name, mismatch)
		}
		return BootSetInfo{}, fail(ErrFailed, "add boot set %q: %v", name, err)
	}
	set, err := s.addedSets.Get(ctx, name)
	if err != nil {
		return BootSetInfo{}, fail(ErrFailed, "read back boot set %q: %v", name, err)
	}
	return BootSetInfo{Name: name, Set: set, Origin: BootSetAdded}, nil
}

func derefInfo(i *BootSetInfo) BootSetInfo {
	if i == nil {
		return BootSetInfo{}
	}
	return *i
}

// offeredAs returns the set the name offers when it is offered with the
// declared digests, nil when it is not offered, and ErrConflict when it is
// offered with others.
func (s *service) offeredAs(ctx context.Context, name string, declared BootSet) (*BootSetInfo, error) {
	set, origin, err := s.resolveBootSet(ctx, name)
	switch {
	case errors.Is(err, ErrBootSetAbsent):
		return nil, nil
	case err != nil:
		return nil, fail(ErrFailed, "read boot set %q: %v", name, err)
	case !set.sameDigests(declared):
		return nil, fail(ErrConflict, "boot set %q is offered with other digests", name)
	}
	return &BootSetInfo{Name: name, Set: set, Origin: origin}, nil
}

func (s *service) ListBootSets(ctx context.Context) ([]BootSetInfo, error) {
	all := make(BootSets, len(s.bootSets))
	origins := make(map[string]BootSetOrigin, len(s.bootSets))
	if s.addedSets != nil {
		added, err := s.addedSets.List(ctx)
		if err != nil {
			return nil, fail(ErrFailed, "list the added boot sets: %v", err)
		}
		for name, set := range added {
			all[name], origins[name] = set, BootSetAdded
		}
	}
	for name, set := range s.bootSets {
		all[name], origins[name] = set, BootSetConfigured
	}
	infos := make([]BootSetInfo, 0, len(all))
	for _, name := range all.Names() {
		infos = append(infos, BootSetInfo{Name: name, Set: all[name], Origin: origins[name]})
	}
	return infos, nil
}

func (s *service) RemoveBootSet(ctx context.Context, name string) error {
	if name == "" {
		return fail(ErrInvalid, "the boot set name is empty")
	}
	if _, ok := s.bootSets[name]; ok {
		return fail(ErrPreconditionFailed, "boot set %q is named in the node's configuration and is not removed through the API", name)
	}
	// Create holds the lock shared from resolving the name until it has set the
	// Machine's record, so a removal that cannot take it now is beside one that
	// is about to name the set. It is refused and not made to wait: the set may
	// be in use by then.
	unlock, ok := s.bootSetLocks.tryLockExclusive(name)
	if !ok {
		return fail(ErrPreconditionFailed, "boot set %q is being resolved by a create call", name)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()

	if machine, ok := s.machineUsingBootSet(name); false && ok {
		return fail(ErrPreconditionFailed, "boot set %q is used by Machine %q", name, machine)
	}
	if s.addedSets == nil {
		return nil
	}
	if err := s.addedSets.Remove(ctx, name); err != nil {
		return fail(ErrFailed, "remove boot set %q: %v", name, err)
	}
	return nil
}

// machineUsingBootSet returns the first, by identifier, of the Machines whose
// record names the boot set, in any state. A failed attempt's record holds no
// Machine and names nothing.
func (s *service) machineUsingBootSet(name string) (MachineID, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var first MachineID
	for id, rec := range s.records {
		if rec.Machine != nil && rec.Machine.OS != nil && rec.Machine.OS.BootSet == name && (first == "" || id < first) {
			first = id
		}
	}
	return first, first != ""
}
