package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

const (
	// firstEndOnFD is the lowest descriptor a description may name for its pipe:
	// the ones below are the supervisor's own (tap, status, console).
	firstEndOnFD = outputFD + 1
	// maxEndOnFD and maxEndOnMatch bound what a request may ask of the
	// supervisor, which holds the descriptor and the carried bytes.
	maxEndOnFD    = 255
	maxEndOnMatch = 256
	// pipeReadSize is the most the pipe is read in one piece.
	pipeReadSize = 4096
)

// checkEndOn refuses a pipe the supervisor cannot give: a descriptor that is not
// above its own or is unreasonably high, or a match that is empty or long.
func checkEndOn(fd uint32, match []byte) error {
	switch {
	case fd < firstEndOnFD || fd > maxEndOnFD:
		return fmt.Errorf("the pipe's descriptor %d is outside %d to %d", fd, firstEndOnFD, maxEndOnFD)
	case len(match) == 0:
		return errors.New("the pipe has no bytes to match")
	case len(match) > maxEndOnMatch:
		return fmt.Errorf("the pipe's match is %d bytes, over %d", len(match), maxEndOnMatch)
	}
	return nil
}

// matcher finds a byte string in a stream fed to it in pieces, holding no more
// than the last len(match)-1 bytes between pieces, so a match that spans two
// pieces is found and the stream's length costs nothing.
type matcher struct {
	match []byte
	buf   []byte
}

// feed reports whether the match is in the bytes fed so far, ending at p.
func (m *matcher) feed(p []byte) bool {
	m.buf = append(m.buf, p...)
	found := bytes.Contains(m.buf, m.match)
	keep := min(len(m.match)-1, len(m.buf))
	m.buf = m.buf[:copy(m.buf, m.buf[len(m.buf)-keep:])]
	return found
}

// watch reads r to its end in pieces of pipeReadSize and, on the first match,
// writes why to log and calls found. It reads on after the match, to the end,
// so that the VMM never blocks on a full pipe.
func watch(r io.Reader, match []byte, found func(), log io.Writer) {
	m := &matcher{match: match}
	buf := make([]byte, pipeReadSize)
	var once sync.Once
	for {
		n, err := r.Read(buf)
		if n > 0 && m.feed(buf[:n]) {
			once.Do(func() {
				fmt.Fprintf(log, "mvmd-supervisor: ending the VMM: its event pipe wrote %q\n", match)
				found()
			})
		}
		if err != nil {
			return
		}
	}
}

// eventPipe is a pipe the VMM writes to as a descriptor of its own, which the
// supervisor reads (ADR 0026).
type eventPipe struct {
	r, w  *os.File
	match []byte
}

// watch closes the write end here, so that the pipe ends with the VMM, and
// reads the pipe in the background to its end. On a match it writes why to log
// and calls end.
func (e *eventPipe) watch(end func(), log io.Writer) {
	e.w.Close()
	go func() {
		defer e.r.Close()
		watch(e.r, e.match, end, log)
	}()
}

// EndOnPipe places the write end of a new pipe at descriptor fd of cmd, after
// the files cmd already passes, and returns the function to call with the
// result of cmd.Start. On a failure that function closes the pipe; otherwise it
// starts the reader, which on a match writes why to log and kills the process
// (ADR 0026). The supervisor and the drivertest harness both call it.
func EndOnPipe(cmd *exec.Cmd, fd int, match []byte, log io.Writer) (func(startErr error), error) {
	if err := checkEndOn(uint32(fd), match); err != nil {
		return nil, err
	}
	if len(cmd.ExtraFiles) > fd-3 {
		return nil, fmt.Errorf("the pipe's descriptor %d is one cmd already passes", fd)
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("make the event pipe: %w", err)
	}
	e := &eventPipe{r: r, w: w, match: match}
	cmd.ExtraFiles = append(append(cmd.ExtraFiles, make([]*os.File, fd-3-len(cmd.ExtraFiles))...), w)
	return func(startErr error) {
		if startErr != nil {
			w.Close()
			r.Close()
			return
		}
		e.watch(func() { _ = cmd.Process.Kill() }, log)
	}, nil
}

// placeEndOn is EndOnPipe for a request's description, which has none when its
// field is empty.
func placeEndOn(cmd *exec.Cmd, e *pb.EndOn, log io.Writer) (func(startErr error), error) {
	if e == nil {
		return func(error) {}, nil
	}
	return EndOnPipe(cmd, int(e.GetFd()), e.GetMatch(), log)
}
