package supervision

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// waitReadable waits until the pidfd is readable, which is when its process
// has terminated.
func waitReadable(pidfd int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		left := time.Until(deadline)
		if left <= 0 {
			return fmt.Errorf("still running after %v", timeout)
		}
		fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int(left.Milliseconds())+1)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}

// waitNamespaceEmpty waits until no process is in the PID namespace whose
// /proc/<pid>/ns/pid link reads ns. The kernel kills the namespace's processes
// when its init ends, and that happens after the init's pidfd turns readable.
func waitNamespaceEmpty(ns string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if !namespaceOccupied(ns) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("processes remain after %v", timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

// namespaceOccupied reports whether a process of the host is in the namespace.
func namespaceOccupied(ns string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		if link, err := os.Readlink("/proc/" + e.Name() + "/ns/pid"); err == nil && link == ns {
			return true
		}
	}
	return false
}
