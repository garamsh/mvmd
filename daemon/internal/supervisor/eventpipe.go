package supervisor

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
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
	tail  []byte
}

// feed reports whether the match is in the bytes fed so far, ending at p.
func (m *matcher) feed(p []byte) bool {
	window := append(append([]byte(nil), m.tail...), p...)
	if bytes.Contains(window, m.match) {
		return true
	}
	keep := min(len(m.match)-1, len(window))
	m.tail = window[len(window)-keep:]
	return false
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

// EventPipe is a pipe the VMM writes to as a descriptor of its own, which the
// supervisor reads (ADR 0026).
type EventPipe struct {
	r, w  *os.File
	match []byte
}

// OpenEventPipe makes the pipe. The caller passes File to the VMM and starts
// Watch once the VMM holds its own descriptor.
func OpenEventPipe(match []byte) (*EventPipe, error) {
	if len(match) == 0 {
		return nil, errors.New("the pipe has no bytes to match")
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	return &EventPipe{r: r, w: w, match: match}, nil
}

// File is the write end, for the VMM.
func (e *EventPipe) File() *os.File { return e.w }

// Close closes both ends, for a pipe that was not watched.
func (e *EventPipe) Close() {
	e.w.Close()
	e.r.Close()
}

// Watch closes the write end here, so that the pipe ends with the VMM, and reads
// the pipe in the background to its end. On a match it writes why to log and
// calls end.
func (e *EventPipe) Watch(end func(), log io.Writer) {
	e.w.Close()
	go func() {
		defer e.r.Close()
		watch(e.r, e.match, end, log)
	}()
}
