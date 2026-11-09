package supervisor

import (
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

const (
	// chunkSize is the most the VMM's output is read in one piece.
	chunkSize = 4096
	// sessionBacklog is how many pieces of output a session may be behind before
	// it is dropped: the log and the other sessions never wait for it.
	sessionBacklog = 256
	// drainTimeout bounds how long the end of the VMM waits for the output it
	// had written to reach the log.
	drainTimeout = 2 * time.Second
)

// Console carries the VMM's standard input and output, which are the serial
// device of a VMM that has one on them: the output goes to the log in full and
// to every session open at the time, and what a session sends is written to the
// input. Sessions are independent of each other and of the log.
type Console struct {
	in  io.Writer
	log io.Writer

	mu       sync.Mutex
	sessions map[*session]struct{}
	ended    bool
	done     chan struct{}
}

type session struct {
	conn net.Conn
	out  chan []byte
}

// ServeConsole accepts sessions on l until the VMM's output ends, and returns
// at once. It reads out to its end, writing every byte to log before any
// session sees it, and closes l and the sessions then. Bytes a session sends
// are written to in.
func ServeConsole(l net.Listener, in io.Writer, out io.Reader, log io.Writer) *Console {
	c := &Console{in: in, log: log, sessions: map[*session]struct{}{}, done: make(chan struct{})}
	go c.accept(l)
	go c.pump(l, out)
	return c
}

// Wait returns when the VMM's output has ended and reached the log, or after
// a bound on how long that may take.
func (c *Console) Wait() {
	select {
	case <-c.done:
	case <-time.After(drainTimeout):
	}
}

func (c *Console) pump(l net.Listener, out io.Reader) {
	buf := make([]byte, chunkSize)
	for {
		n, err := out.Read(buf)
		if n > 0 {
			_, _ = c.log.Write(buf[:n])
			c.broadcast(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			break
		}
	}
	_ = l.Close()
	c.mu.Lock()
	c.ended = true
	for s := range c.sessions {
		delete(c.sessions, s)
		close(s.out)
	}
	c.mu.Unlock()
	close(c.done)
}

// broadcast hands the piece to every session, dropping one that is too far
// behind to take it.
func (c *Console) broadcast(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.sessions {
		select {
		case s.out <- p:
		default:
			delete(c.sessions, s)
			close(s.out)
			_ = s.conn.Close() // ends a send blocked on a session that reads nothing
		}
	}
}

func (c *Console) accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		s := &session{conn: conn, out: make(chan []byte, sessionBacklog)}
		c.mu.Lock()
		if c.ended {
			c.mu.Unlock()
			_ = conn.Close()
			continue
		}
		c.sessions[s] = struct{}{}
		c.mu.Unlock()
		go c.send(s)
		go c.receive(s)
	}
}

// send writes the session's output to it, and closes it when the output ends
// or the session was dropped.
func (c *Console) send(s *session) {
	defer s.conn.Close()
	for p := range s.out {
		if _, err := s.conn.Write(p); err != nil {
			c.drop(s)
			return
		}
	}
}

// receive writes what the session sends to the VMM's input, until the session
// ends.
func (c *Console) receive(s *session) {
	_, _ = io.Copy(c.in, s.conn)
	c.drop(s)
	_ = s.conn.Close()
}

func (c *Console) drop(s *session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sessions[s]; ok {
		delete(c.sessions, s)
		close(s.out)
	}
}

// consolePipes are the four ends of the two pipes that are the VMM's standard
// input and output. They are two pipes, not one socket, since a VMM that makes
// its input non-blocking would make its output so too if one open file were
// both.
type consolePipes struct {
	vmmIn, consoleIn, consoleOut, vmmOut *os.File
}

func newConsolePipes() (*consolePipes, error) {
	p := &consolePipes{}
	var err error
	if p.vmmIn, p.consoleIn, err = os.Pipe(); err != nil {
		return nil, err
	}
	if p.consoleOut, p.vmmOut, err = os.Pipe(); err != nil {
		p.closeVMM()
		p.consoleIn.Close()
		return nil, err
	}
	return p, nil
}

// closeVMM closes the ends the VMM holds, in the supervisor once the VMM has
// its own.
func (p *consolePipes) closeVMM() {
	p.vmmIn.Close()
	p.vmmOut.Close()
}

func (p *consolePipes) closeConsole() {
	p.consoleIn.Close()
	p.consoleOut.Close()
}

// listenConsole makes the console socket, readable by its owner alone. The
// state directory it is in is the supervisor's, so the VMM, whose run directory
// is another, cannot replace it.
func listenConsole(path string) (net.Listener, error) {
	_ = os.Remove(path)
	l, err := unixsock.Listen(path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = l.Close()
		return nil, err
	}
	return l, nil
}
