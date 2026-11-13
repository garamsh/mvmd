package memory

import "sync"

// BootIdentity is an in-memory machine.BootIdentity that changes only when
// told to.
type BootIdentity struct {
	mu sync.Mutex
	id string
}

// NewBootIdentity returns a BootIdentity of the boot named id.
func NewBootIdentity(id string) *BootIdentity { return &BootIdentity{id: id} }

// BootID implements machine.BootIdentity.
func (b *BootIdentity) BootID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.id
}

// Reboot starts a new boot of the given identity.
func (b *BootIdentity) Reboot(id string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.id = id
}
