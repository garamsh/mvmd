package supervisor

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

const (
	// chunkSize is the most the VMM's output is read in one piece.
	chunkSize = 4096
	// sessionBacklog is how many bytes of output a session may be behind before
	// it is dropped: the log and the other sessions never wait for it. It is
	// bytes and not pieces because the terminal gives the output in pieces as
	// small as the VMM writes them, and a Unix socket holds few of those.
	sessionBacklog = 256 * chunkSize
	// drainTimeout bounds how long the end of the VMM waits for the output it
	// had written to reach the log.
	drainTimeout = 2 * time.Second
)

// Console carries the VMM's standard input and output, which are the serial
// device of a VMM that has one on them (ADR 0025): the output goes to the log in full and
// to every session open at the time, and what a session sends is written to the
// input. Sessions are independent of each other and of the log.
type Console struct {
	in  io.Writer
	log io.Writer

	// events is where the console says why a session or its output ended.
	events *slog.Logger

	// end closes the terminal the console reads, for a console that was opened
	// on one; nil for one served over other streams.
	end io.Closer

	mu       sync.Mutex
	sessions map[*session]struct{}
	ended    bool
	done     chan struct{}
}

// session is one client of the console. The pump queues output on it and its
// send goroutine writes the queue to the connection in as few writes as it can.
type session struct {
	conn net.Conn
	wake chan struct{} // holds one token when the queue or closed has changed

	mu      sync.Mutex
	pending []byte
	closed  bool // nothing more is queued; send ends once pending is written
}

// push queues p, and reports false, queueing nothing, if the session would
// be more than sessionBacklog behind.
func (s *session) push(p []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending)+len(p) > sessionBacklog {
		return false
	}
	s.pending = append(s.pending, p...)
	s.signal()
	return true
}

// finish ends the session after what is queued has been written.
func (s *session) finish() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.signal()
}

// abort ends the session and discards what is queued.
func (s *session) abort() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed, s.pending = true, nil
	s.signal()
}

// take returns what is queued and whether the session was finished at that
// moment.
func (s *session) take() (p []byte, closed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, s.pending = s.pending, nil
	return p, s.closed
}

func (s *session) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ServeConsole accepts sessions on l until the VMM's output ends, and returns
// at once. It reads out to its end, writing every byte to log before any
// session sees it, and closes l and the sessions then. Bytes a session sends
// are written to in.
func ServeConsole(l net.Listener, in io.Writer, out io.Reader, log io.Writer) *Console {
	return serveConsole(l, in, out, log, slog.Default())
}

// serveConsole is ServeConsole with the logger that names why a session or the
// output ended.
func serveConsole(l net.Listener, in io.Writer, out io.Reader, log io.Writer, events *slog.Logger) *Console {
	c := &Console{in: in, log: log, events: events, sessions: map[*session]struct{}{}, done: make(chan struct{})}
	go c.accept(l)
	go c.pump(l, out)
	return c
}

// OpenConsole makes the VMM's terminal (ADR 0025) and serves it on the console
// socket in stateDir, with the VMM's output written to log first. It returns
// the console and the two descriptors that are the VMM's standard input and
// output, which the caller closes once the VMM holds its own, and which are
// the only way the VMM reaches the terminal.
func OpenConsole(stateDir string, log io.Writer) (c *Console, in, out *os.File, err error) {
	l, err := listenConsole(statedir.ConsoleSocket(stateDir))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("listen on the console socket: %w", err)
	}
	t, in, out, err := openTerminal()
	if err != nil {
		_ = l.Close()
		return nil, nil, nil, err
	}
	c = ServeConsole(l, t, t, log)
	c.end = t
	return c, in, out, nil
}

// Close ends a console opened by OpenConsole that has no VMM to serve: its
// output ends, and with it the socket and the sessions.
func (c *Console) Close() error {
	if c.end == nil {
		return nil
	}
	return c.end.Close()
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
	var total int64
	var err error
	for {
		var n int
		n, err = out.Read(buf)
		if n > 0 {
			total += int64(n)
			_, _ = c.log.Write(buf[:n])
			c.broadcast(buf[:n])
		}
		if err != nil {
			break
		}
	}
	_ = l.Close()
	c.mu.Lock()
	c.ended = true
	open := len(c.sessions)
	for s := range c.sessions {
		delete(c.sessions, s)
		s.finish()
	}
	c.mu.Unlock()
	c.events.Info("console output ended", append(outputEnd(out, err), "bytes", total, "sessions_ended", open)...)
	close(c.done)
}

// outputEnd is the log attributes that say why the VMM's output ended: the end
// of the stream, the controlling side's EIO (which terminal.Read reports as
// the end), the output closed by Console.Close, or a read error with its errno.
func outputEnd(out io.Reader, err error) []any {
	switch {
	case errors.Is(err, io.EOF):
		if t, ok := out.(interface{ endedByEIO() bool }); ok && t.endedByEIO() {
			return []any{"reason", "eio on the terminal: no process holds its subordinate side"}
		}
		return []any{"reason", "end of the output"}
	case errors.Is(err, os.ErrClosed):
		return []any{"reason", "closed by the supervisor"}
	}
	attrs := []any{"reason", "read error", "err", err}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		attrs = append(attrs, "errno", int(errno))
	}
	return attrs
}

// broadcast hands the piece to every session, dropping one that is too far
// behind to take it. The sessions copy the piece.
func (c *Console) broadcast(p []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for s := range c.sessions {
		if !s.push(p) {
			delete(c.sessions, s)
			s.abort()
			_ = s.conn.Close() // ends a send blocked on a session that reads nothing
			c.events.Info("console session dropped", "reason", "backlog", "backlog_bytes", sessionBacklog)
		}
	}
}

func (c *Console) accept(l net.Listener) {
	for {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		s := &session{conn: conn, wake: make(chan struct{}, 1)}
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
	for {
		<-s.wake
		p, closed := s.take()
		if len(p) > 0 {
			if _, err := s.conn.Write(p); err != nil {
				c.drop(s, "write to the session failed", err)
				return
			}
		}
		if closed {
			return
		}
	}
}

// receive writes what the session sends to the VMM's input, until the session
// ends.
func (c *Console) receive(s *session) {
	_, err := io.Copy(c.in, s.conn)
	reason := "the client closed the session"
	if err != nil {
		reason = "copy between the session and the VMM's input failed"
	}
	c.drop(s, reason, err)
	_ = s.conn.Close()
}

// drop removes the session if it is still open and logs why.
func (c *Console) drop(s *session, reason string, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.sessions[s]; ok {
		delete(c.sessions, s)
		s.abort()
		attrs := []any{"reason", reason}
		if err != nil {
			attrs = append(attrs, "err", err)
		}
		c.events.Info("console session ended", attrs...)
	}
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
