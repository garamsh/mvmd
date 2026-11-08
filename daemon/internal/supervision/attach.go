package supervision

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// attached is mvmd's hold on one live supervisor and its VMM: a pidfd to each,
// taken from the supervisor's socket, so that neither can be confused with a
// process that reuses its number (ADR 0003).
type attached struct {
	pid   int
	ns    string
	supFD int
	// vmmFD is -1 while the Machine has no VMM process, or a supervisor of an
	// earlier release did not report it.
	vmmFD  int
	vmmPID int
}

func (h *attached) close() {
	unix.Close(h.supFD)
	if h.vmmFD >= 0 {
		unix.Close(h.vmmFD)
	}
}

// attach takes the pidfds of the supervisor listening on the socket and of its
// VMM. It returns an error satisfying isAbsent when nobody listens.
func attach(ctx context.Context, sock string) (*attached, error) {
	pid, err := peerPID(sock)
	if err != nil {
		return nil, err
	}
	h := &attached{pid: pid, vmmFD: -1}
	if h.ns, err = os.Readlink(fmt.Sprintf("/proc/%d/ns/pid", pid)); err != nil {
		return nil, fmt.Errorf("read the namespace of supervisor %d: %w", pid, err)
	}
	if h.supFD, err = unix.PidfdOpen(pid, 0); err != nil {
		return nil, fmt.Errorf("open a pidfd to supervisor %d: %w", pid, err)
	}
	var inner int32
	// A supervisor that does not answer, or one of an earlier release that
	// reports no VMM, leaves the VMM without a pidfd: the supervisor's own end
	// still ends its namespace.
	_ = withClient(ctx, sock, func(ctx context.Context, c pb.SupervisorServiceClient) error {
		resp, err := c.Inspect(ctx, &pb.InspectRequest{})
		if err == nil {
			inner = resp.GetVmmPid()
		}
		return err
	})
	if inner > 0 {
		if host, ok := hostPID(pid, int(inner)); ok {
			if fd, err := unix.PidfdOpen(host, 0); err == nil {
				// The id may have been reused between the scan and the open:
				// the pidfd is kept only if the id still names the child.
				if stillTheChild(host, pid, int(inner)) {
					h.vmmFD, h.vmmPID = fd, host
				} else {
					unix.Close(fd)
				}
			}
		}
	}
	return h, nil
}

// hostPID returns the id, in mvmd's namespace, of the child of the supervisor
// that the supervisor numbers inner. The VMM is the supervisor's own child, and
// /proc/<pid>/status shows each process's parent and its ids in every
// namespace it is in, from the reader's down to its own.
func hostPID(supervisor, inner int) (int, bool) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, false
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if stillTheChild(pid, supervisor, inner) {
			return pid, true
		}
	}
	return 0, false
}

// stillTheChild reports whether pid is the child of supervisor that the
// supervisor numbers inner, read from /proc/<pid>/status. A process that is
// gone is not.
func stillTheChild(pid, supervisor, inner int) bool {
	raw, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return false
	}
	var ppid int
	var nspid []string
	for _, line := range strings.Split(string(raw), "\n") {
		switch {
		case strings.HasPrefix(line, "PPid:"):
			ppid, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "PPid:")))
		case strings.HasPrefix(line, "NSpid:"):
			nspid = strings.Fields(strings.TrimPrefix(line, "NSpid:"))
		}
	}
	return ppid == supervisor && len(nspid) > 1 && nspid[len(nspid)-1] == strconv.Itoa(inner)
}
