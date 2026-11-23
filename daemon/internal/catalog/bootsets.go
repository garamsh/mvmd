package catalog

import (
	"context"
	"errors"
	"regexp"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var (
	bootSetName   = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)
	bootSetDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

func sameDigests(b, o machine.BootSet) bool {
	return b.Kernel.SHA256 == o.Kernel.SHA256 && b.Initrd.SHA256 == o.Initrd.SHA256
}

// resolveBootSet finds the set the name offers, a configured one before an
// added one, and reports its origin. A name that is not offered is
// machine.ErrBootSetAbsent.
func (s *service) resolveBootSet(ctx context.Context, name string) (machine.BootSet, machine.BootSetOrigin, error) {
	if set, ok := s.bootSets[name]; ok {
		return set, machine.BootSetConfigured, nil
	}
	if s.addedSets == nil {
		return machine.BootSet{}, "", machine.ErrBootSetAbsent
	}
	set, err := s.addedSets.Get(ctx, name)
	return set, machine.BootSetAdded, err
}

func (s *service) AddBootSet(ctx context.Context, name string, upload machine.BootUpload) (machine.BootSetInfo, error) {
	declared := machine.BootSet{Kernel: machine.KernelFile{SHA256: upload.Kernel.SHA256}, Initrd: machine.KernelFile{SHA256: upload.Initrd.SHA256}}
	switch {
	case !bootSetName.MatchString(name):
		return machine.BootSetInfo{}, fail(machine.ErrInvalid, "boot set name %q is not 1 to 63 characters of a-z, 0-9 and \"-\"", name)
	case !bootSetDigest.MatchString(declared.Kernel.SHA256):
		return machine.BootSetInfo{}, fail(machine.ErrInvalid, "the kernel digest %q is not 64 lowercase hexadecimal digits", declared.Kernel.SHA256)
	case !bootSetDigest.MatchString(declared.Initrd.SHA256):
		return machine.BootSetInfo{}, fail(machine.ErrInvalid, "the initrd digest %q is not 64 lowercase hexadecimal digits", declared.Initrd.SHA256)
	}
	if s.addedSets == nil {
		return machine.BootSetInfo{}, fail(machine.ErrUnsupported, "the node keeps no boot set a client adds")
	}

	// The caller's own context bounds the upload: a stream that breaks ends
	// the call with nothing published.
	held, err := s.offeredAs(ctx, name, declared)
	if err != nil || held != nil {
		return derefInfo(held), err
	}
	switch err := s.addedSets.Add(ctx, name, upload); {
	case err == nil:
	case errors.Is(err, machine.ErrBootSetExists):
		// Another add of the name published first.
		held, err := s.offeredAs(ctx, name, declared)
		if err != nil {
			return machine.BootSetInfo{}, err
		}
		return derefInfo(held), nil
	default:
		var mismatch *machine.BootFileMismatch
		if errors.As(err, &mismatch) {
			return machine.BootSetInfo{}, fail(machine.ErrInvalid, "boot set %q: %v", name, mismatch)
		}
		return machine.BootSetInfo{}, fail(machine.ErrFailed, "add boot set %q: %v", name, err)
	}
	set, err := s.addedSets.Get(ctx, name)
	if err != nil {
		return machine.BootSetInfo{}, fail(machine.ErrFailed, "read back boot set %q: %v", name, err)
	}
	return machine.BootSetInfo{Name: name, Set: set, Origin: machine.BootSetAdded}, nil
}

func derefInfo(i *machine.BootSetInfo) machine.BootSetInfo {
	if i == nil {
		return machine.BootSetInfo{}
	}
	return *i
}

// offeredAs returns the set the name offers when it is offered with the
// declared digests, nil when it is not offered, and machine.ErrConflict when it is
// offered with others.
func (s *service) offeredAs(ctx context.Context, name string, declared machine.BootSet) (*machine.BootSetInfo, error) {
	set, origin, err := s.resolveBootSet(ctx, name)
	switch {
	case errors.Is(err, machine.ErrBootSetAbsent):
		return nil, nil
	case err != nil:
		return nil, fail(machine.ErrFailed, "read boot set %q: %v", name, err)
	case !sameDigests(set, declared):
		return nil, fail(machine.ErrConflict, "boot set %q is offered with other digests", name)
	}
	return &machine.BootSetInfo{Name: name, Set: set, Origin: origin}, nil
}

func (s *service) ListBootSets(ctx context.Context) ([]machine.BootSetInfo, error) {
	all := make(machine.BootSets, len(s.bootSets))
	origins := make(map[string]machine.BootSetOrigin, len(s.bootSets))
	if s.addedSets != nil {
		added, err := s.addedSets.List(ctx)
		if err != nil {
			return nil, fail(machine.ErrFailed, "list the added boot sets: %v", err)
		}
		for name, set := range added {
			all[name], origins[name] = set, machine.BootSetAdded
		}
	}
	for name, set := range s.bootSets {
		all[name], origins[name] = set, machine.BootSetConfigured
	}
	infos := make([]machine.BootSetInfo, 0, len(all))
	for _, name := range all.Names() {
		infos = append(infos, machine.BootSetInfo{Name: name, Set: all[name], Origin: origins[name]})
	}
	return infos, nil
}

func (s *service) RemoveBootSet(ctx context.Context, name string) error {
	if name == "" {
		return fail(machine.ErrInvalid, "the boot set name is empty")
	}
	if _, ok := s.bootSets[name]; ok {
		return fail(machine.ErrPreconditionFailed, "boot set %q is named in the node's configuration and is not removed through the API", name)
	}
	// Create holds the lock shared from resolving the name until it has set the
	// Machine's record, so a removal that cannot take it now is beside one that
	// is about to name the set. It is refused and not made to wait: the set may
	// be in use by then.
	unlock, ok := s.bootSetLocks.TryLockExclusive(name)
	if !ok {
		return fail(machine.ErrPreconditionFailed, "boot set %q is being resolved by a create call", name)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx)
	defer cancel()

	records, err := s.store.Load(ctx)
	if err != nil {
		return fail(machine.ErrFailed, "read the Machine records: %v", err)
	}
	if id, ok := machineUsingBootSet(records, name); ok {
		return fail(machine.ErrPreconditionFailed, "boot set %q is used by Machine %q", name, id)
	}
	if s.addedSets == nil {
		return nil
	}
	if err := s.addedSets.Remove(ctx, name); err != nil {
		return fail(machine.ErrFailed, "remove boot set %q: %v", name, err)
	}
	return nil
}

// machineUsingBootSet returns the first, by identifier, of the Machines whose
// record names the boot set, in any state. A failed attempt's record holds no
// Machine and names nothing, and a record that could not be read holds none
// either, as the lifecycle reads it.
func machineUsingBootSet(records []machine.Record, name string) (machine.MachineID, bool) {
	var first machine.MachineID
	for _, rec := range records {
		if rec.Machine != nil && rec.Unreadable == "" && rec.Machine.OS != nil && rec.Machine.OS.BootSet == name && (first == "" || rec.ID < first) {
			first = rec.ID
		}
	}
	return first, first != ""
}

// ResolveBootSet implements machine.Catalog.
func (s *service) ResolveBootSet(ctx context.Context, name string) (machine.BootSet, error) {
	set, _, err := s.resolveBootSet(ctx, name)
	return set, err
}

// HoldBootSet implements machine.Catalog.
func (s *service) HoldBootSet(name string) func() {
	return s.bootSetLocks.Lock(name, false)
}
