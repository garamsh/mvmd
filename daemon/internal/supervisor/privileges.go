package supervisor

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// dropPrivileges empties the bounding and ambient capability sets and changes
// the calling thread to the user and group, with no supplementary groups, in
// that order (ADR 0015): after it the user ids are all nonzero, so nothing is
// permitted or effective, and an exec adds nothing. It then reads the result
// back and fails if the thread still holds a capability or the user is not the
// one asked for.
func dropPrivileges(uid, gid uint32) error {
	for c := 0; ; c++ {
		err := unix.Prctl(unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0)
		if errors.Is(err, unix.EINVAL) {
			break // c is past the last capability the kernel has
		}
		if err != nil {
			return fmt.Errorf("drop capability %d from the bounding set: %w", c, err)
		}
	}
	if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
		return fmt.Errorf("clear the ambient set: %w", err)
	}
	if err := unix.Setgroups(nil); err != nil {
		return fmt.Errorf("drop the supplementary groups: %w", err)
	}
	if err := unix.Setresgid(int(gid), int(gid), int(gid)); err != nil {
		return fmt.Errorf("change the group to %d: %w", gid, err)
	}
	if err := unix.Setresuid(int(uid), int(uid), int(uid)); err != nil {
		return fmt.Errorf("change the user to %d: %w", uid, err)
	}
	// The inheritable set survives a change of user, so it is emptied too.
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return fmt.Errorf("empty the inheritable set: %w", err)
	}
	return checkDropped(uid, gid)
}

// checkDropped reads the thread's user and capabilities back.
func checkDropped(uid, gid uint32) error {
	if r, e, s := unix.Getresuid(); r != int(uid) || e != int(uid) || s != int(uid) {
		return fmt.Errorf("the thread's user ids are %d %d %d, not %d", r, e, s, uid)
	}
	if r, e, s := unix.Getresgid(); r != int(gid) || e != int(gid) || s != int(gid) {
		return fmt.Errorf("the thread's group ids are %d %d %d, not %d", r, e, s, gid)
	}
	if groups, err := unix.Getgroups(); err != nil || len(groups) != 0 {
		return fmt.Errorf("the thread has supplementary groups %v (%v)", groups, err)
	}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return fmt.Errorf("read the capabilities back: %w", err)
	}
	for _, d := range data {
		if d.Effective|d.Permitted|d.Inheritable != 0 {
			return fmt.Errorf("the thread still holds capabilities: %v", data)
		}
	}
	return nil
}
