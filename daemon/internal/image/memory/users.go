package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// FirstUID is the lowest UID Users hands out.
const FirstUID uint32 = 200000

// Users is an in-memory image.Users. Its step is "allocate".
type Users struct {
	machinememory.Failures

	mu   sync.Mutex
	held map[uint32]bool
}

// NewUsers returns Users that holds no user.
func NewUsers() *Users {
	return &Users{held: make(map[uint32]bool)}
}

// Allocate implements image.Users: the lowest free UID, with the GID of the
// same number.
func (u *Users) Allocate(context.Context) (machine.User, error) {
	if err := u.Enter("allocate"); err != nil {
		return machine.User{}, err
	}
	u.mu.Lock()
	defer u.mu.Unlock()
	uid := FirstUID
	for u.held[uid] {
		uid++
	}
	u.held[uid] = true
	return machine.User{UID: uid, GID: uid}, nil
}

// Release implements image.Users.
func (u *Users) Release(user machine.User) {
	u.mu.Lock()
	defer u.mu.Unlock()
	delete(u.held, user.UID)
}

// Held returns how many users are allocated and not released.
func (u *Users) Held() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.held)
}
