package supervisor

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// boottime returns the host's CLOCK_BOOTTIME in nanoseconds since boot.
func boottime() (int64, error) {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return 0, fmt.Errorf("read CLOCK_BOOTTIME: %w", err)
	}
	return ts.Nano(), nil
}

// timer is the expiry as a timerfd on CLOCK_BOOTTIME, armed with an absolute
// time so that delivery delay cannot lengthen it (ADR 0003).
type timer struct {
	fd int
}

func newTimer() (*timer, error) {
	fd, err := unix.TimerfdCreate(unix.CLOCK_BOOTTIME, unix.TFD_CLOEXEC)
	if err != nil {
		return nil, fmt.Errorf("create the timerfd: %w", err)
	}
	return &timer{fd: fd}, nil
}

// arm sets the absolute expiry. An expiry already passed fires at once.
func (t *timer) arm(expiry int64) error {
	spec := unix.ItimerSpec{Value: unix.NsecToTimespec(expiry)}
	if err := unix.TimerfdSettime(t.fd, unix.TFD_TIMER_ABSTIME, &spec, nil); err != nil {
		return fmt.Errorf("arm the timerfd: %w", err)
	}
	return nil
}

// enforce makes the calling thread the one that counts: it locks the thread,
// gives it the real-time policy with reset-on-fork so that no process started
// from it inherits the policy, and then waits for the expiry and exits the
// process, which ends the namespace. It returns the outcome of its setup on
// ready and does not return afterwards.
func (t *timer) enforce(ready chan<- error) {
	runtime.LockOSThread()
	attr := unix.SchedAttr{
		Size:     uint32(unsafe.Sizeof(unix.SchedAttr{})),
		Policy:   unix.SCHED_FIFO,
		Flags:    unix.SCHED_FLAG_RESET_ON_FORK,
		Priority: 1,
	}
	if err := unix.SchedSetAttr(0, &attr, 0); err != nil {
		ready <- fmt.Errorf("set the real-time policy: %w", err)
		return
	}
	ready <- nil

	var buf [8]byte
	for {
		_, err := unix.Read(t.fd, buf[:])
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "mvmd-supervisor: read the timerfd: %v\n", err)
		}
		// The expiry. Exiting is the whole action: the kernel kills every
		// process of the namespace with its init.
		os.Exit(0)
	}
}

// lockMemory locks the supervisor's pages so that its path to exit takes no
// page-in.
func lockMemory() error {
	if err := unix.Mlockall(unix.MCL_CURRENT | unix.MCL_FUTURE); err != nil {
		return fmt.Errorf("lock the memory: %w", err)
	}
	return nil
}
