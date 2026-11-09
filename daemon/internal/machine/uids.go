package machine

import "sync"

// uidHolder names who holds a UID. A Machine and a conversion are different
// kinds, so an identifier of one never equals an identifier of the other.
type uidHolder struct {
	conversion bool
	name       string
}

// UIDs allocates the VMM users of one node from its one range (ADR 0015). The
// lifecycle draws a Machine's user from it and the image domain draws a
// conversion's, so no two holders share a UID. It is safe for concurrent use.
type UIDs struct {
	rng UIDRange

	mu       sync.Mutex
	byHolder map[uidHolder]uint32
	byUID    map[uint32]uidHolder
}

// NewUIDs returns an allocator over the range, holding no UID.
func NewUIDs(rng UIDRange) *UIDs {
	return &UIDs{rng: rng, byHolder: make(map[uidHolder]uint32), byUID: make(map[uint32]uidHolder)}
}

// allocate gives the holder the lowest free UID in the range.
func (u *UIDs) allocate(h uidHolder) (uint32, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for i := uint32(0); i < u.rng.Count; i++ {
		uid := u.rng.First + i
		if _, occupied := u.byUID[uid]; !occupied {
			u.byHolder[h] = uid
			u.byUID[uid] = h
			return uid, nil
		}
	}
	return 0, fail(ErrPreconditionFailed, "the UID range is exhausted")
}

func (u *UIDs) release(h uidHolder) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if uid, ok := u.byHolder[h]; ok {
		delete(u.byHolder, h)
		delete(u.byUID, uid)
	}
}

// AllocateMachine gives the Machine the lowest free UID in the range.
func (u *UIDs) AllocateMachine(id MachineID) (uint32, error) {
	return u.allocate(uidHolder{name: string(id)})
}

// ReleaseMachine returns the Machine's UID to the range.
func (u *UIDs) ReleaseMachine(id MachineID) {
	u.release(uidHolder{name: string(id)})
}

// AllocateConversion gives the conversion of the image the lowest free UID in
// the range, as the user whose UID and GID are equal.
func (u *UIDs) AllocateConversion(image ImageDigest) (User, error) {
	uid, err := u.allocate(uidHolder{conversion: true, name: string(image)})
	if err != nil {
		return User{}, err
	}
	return User{UID: uid, GID: uid}, nil
}

// ReleaseConversion returns the conversion's UID to the range. The caller
// releases it only once no VMM process of the user exists.
func (u *UIDs) ReleaseConversion(image ImageDigest) {
	u.release(uidHolder{conversion: true, name: string(image)})
}

// Held returns how many UIDs are allocated and not released.
func (u *UIDs) Held() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.byUID)
}

// restoreMachine marks the UID a restored record carries as held by its
// Machine. It fails for a UID outside the range or already held.
func (u *UIDs) restoreMachine(id MachineID, uid uint32) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	if !u.rng.Contains(uid) {
		return fail(ErrPreconditionFailed,
			"restored record %q carries UID %d outside the configured range (first: %d, count: %d)",
			id, uid, u.rng.First, u.rng.Count)
	}
	if other, collides := u.byUID[uid]; collides {
		return fail(ErrPreconditionFailed, "restored records %q and %q share UID %d", other.name, id, uid)
	}
	h := uidHolder{name: string(id)}
	u.byHolder[h] = uid
	u.byUID[uid] = h
	return nil
}
