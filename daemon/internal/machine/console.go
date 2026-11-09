package machine

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
)

// consoleLinePrefix begins every line the Guest Agent writes to the console
// (guest.md, "The console lines").
const consoleLinePrefix = "mvmd-guest-agent: "

// maxConsoleLine bounds a line of the console log that is looked at; a longer
// one is not the agent's, whose lines are short, and is skipped.
const maxConsoleLine = 64 << 10

// Console opens a session to the console of the Running or Paused Machine. It
// takes no lock on the Machine and does not detach from the caller's context:
// the session is a read path that changes no state, so there is no step to
// finish for a caller that has gone, and the session ends with the caller's
// context, with the Machine's VMM, or when the caller closes it.
func (s *service) Console(ctx context.Context, id MachineID) (io.ReadWriteCloser, error) {
	rec, err := s.machine(id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	state := rec.Machine.State
	s.mu.Unlock()
	if state != StateRunning && state != StatePaused {
		return nil, fail(ErrPreconditionFailed, "Machine %q is %s; console needs Running or Paused", id, state)
	}
	session, err := s.driver.Console(ctx, id)
	if errors.Is(err, ErrConsoleUnavailable) {
		// The Machine's VMM ended after the state was read, which takes no lock:
		// the Machine is no longer in a state that allows the call.
		return nil, fail(ErrPreconditionFailed, "Machine %q was %s but its console is gone; console needs Running or Paused", id, state)
	}
	if err != nil {
		return nil, fail(ErrFailed, "open the console: %v", err)
	}
	return endWith(ctx, session), nil
}

// endedSession closes its session once, however it is ended.
type endedSession struct {
	io.ReadWriteCloser
	once sync.Once
	err  error
	stop func() bool
}

// endWith returns the session, which ctx ending closes.
func endWith(ctx context.Context, session io.ReadWriteCloser) io.ReadWriteCloser {
	e := &endedSession{ReadWriteCloser: session}
	e.stop = context.AfterFunc(ctx, func() { _ = e.Close() })
	return e
}

func (e *endedSession) Close() error {
	e.once.Do(func() {
		e.stop()
		e.err = e.ReadWriteCloser.Close()
	})
	return e.err
}

// consoleOutcome ranks the console lines that say how the entrypoint ended: a
// line of a higher rank is what the agent knew of the entrypoint, and decides
// over the lines of a lower one, which only say that the agent itself failed.
type consoleOutcome struct {
	rank   int
	record ExecRecord
}

// parseEntrypointEnd reads the lines the Guest Agent wrote to the console and
// returns the exec record they give for the entrypoint, or false when none of
// them speaks of it. Lines that are not the agent's, and lines of a form the
// agent does not write, are ignored. Where several lines speak, the one that
// knows most of the entrypoint decides, the first of them when two know as
// much.
func parseEntrypointEnd(log io.Reader) (ExecRecord, bool, error) {
	var best *consoleOutcome
	r := bufio.NewReaderSize(log, 4096)
	for {
		line, err := readConsoleLine(r)
		if line != "" {
			if o, ok := parseConsoleLine(line); ok && (best == nil || o.rank > best.rank) {
				best = &o
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return ExecRecord{}, false, err
		}
	}
	if best == nil {
		return ExecRecord{}, false, nil
	}
	return best.record, true, nil
}

// readConsoleLine returns the next line without its line end (LF or CR LF), and an empty one
// for a line longer than maxConsoleLine, whose rest is dropped.
func readConsoleLine(r *bufio.Reader) (string, error) {
	var line []byte
	skipped := false
	for {
		part, isPrefix, err := r.ReadLine()
		if !skipped {
			line = append(line, part...)
			if len(line) > maxConsoleLine {
				line, skipped = nil, true
			}
		}
		if err != nil || !isPrefix {
			if skipped {
				return "", err
			}
			return string(line), err
		}
	}
}

// parseConsoleLine reads one line of the forms guest.md lists.
func parseConsoleLine(line string) (consoleOutcome, bool) {
	text, ok := strings.CutPrefix(line, consoleLinePrefix)
	if !ok {
		return consoleOutcome{}, false
	}
	const entrypointRank, bootRank, failureRank = 3, 2, 1
	exec := func(r ExecRecord) ExecRecord { r.ID = entrypointExec; return r }
	switch {
	case strings.HasPrefix(text, "entrypoint exited code="):
		n, err := strconv.ParseInt(strings.TrimPrefix(text, "entrypoint exited code="), 10, 32)
		if err != nil {
			return consoleOutcome{}, false
		}
		return consoleOutcome{entrypointRank, exec(ExecRecord{Status: ExecExited, ExitCode: int32(n)})}, true
	case strings.HasPrefix(text, "entrypoint exited signal="):
		n, err := strconv.ParseInt(strings.TrimPrefix(text, "entrypoint exited signal="), 10, 32)
		if err != nil || n <= 0 {
			return consoleOutcome{}, false
		}
		return consoleOutcome{entrypointRank, exec(ExecRecord{Status: ExecExited, TermSignal: int32(n)})}, true
	case strings.HasPrefix(text, "entrypoint not started reason="):
		reason := strings.TrimPrefix(text, "entrypoint not started reason=")
		return consoleOutcome{entrypointRank, exec(ExecRecord{Status: ExecNotStarted, Reason: reason})}, true
	case strings.HasPrefix(text, "entrypoint ended reason="):
		reason := strings.TrimPrefix(text, "entrypoint ended reason=")
		return consoleOutcome{entrypointRank, exec(ExecRecord{Status: ExecCutShort, Reason: reason})}, true
	case text == "terminated before the entrypoint ran":
		return consoleOutcome{entrypointRank, exec(ExecRecord{Status: ExecNotStarted,
			Reason: "the guest was terminated before the entrypoint ran"})}, true
	case strings.HasPrefix(text, "boot failed step="):
		step, detail, found := strings.Cut(strings.TrimPrefix(text, "boot failed step="), " detail=")
		if !found || step == "" {
			return consoleOutcome{}, false
		}
		return consoleOutcome{bootRank, exec(ExecRecord{Status: ExecNotStarted,
			Reason: fmt.Sprintf("the guest's boot failed at %s: %s", step, detail)})}, true
	case strings.HasPrefix(text, "failed to "), strings.HasPrefix(text, "panic: "):
		return consoleOutcome{failureRank, exec(ExecRecord{Status: ExecCutShort,
			Reason: "the Guest Agent ended: " + text})}, true
	}
	return consoleOutcome{}, false
}

// entrypointFromLog is the record of the entrypoint that the console log of a
// Machine gives; false when the log says nothing of it.
func (s *service) entrypointFromLog(ctx context.Context, id MachineID) (ExecRecord, bool, error) {
	log, err := s.driver.ConsoleLog(ctx, id)
	if err != nil {
		return ExecRecord{}, false, err
	}
	defer log.Close()
	return parseEntrypointEnd(log)
}

// readEntrypoint answers a read of the entrypoint's exec record. While the
// Machine runs the guest holds the record and is asked. When the Machine's VMM
// has ended, or the guest can no longer be reached because the agent powers
// the Machine off after the entrypoint, the record is what the agent wrote to
// the console, which the Driver's log holds; the output is not, and no chunk
// is sent from it.
func (s *service) readEntrypoint(ctx context.Context, id MachineID, after uint64, send func(OutputChunk) error) (ExecRecord, error) {
	rec, err := s.machine(id)
	if err != nil {
		return ExecRecord{}, err
	}
	s.mu.Lock()
	state := rec.Machine.State
	s.mu.Unlock()

	var unreached error
	switch state {
	case StateRunning:
		record, err := s.guest.ReadExecOutput(ctx, id, entrypointExec, after, send)
		if err == nil {
			record.ID = entrypointExec
			return record, nil
		}
		var gone *OutputGoneError
		switch {
		case errors.As(err, &gone):
			return ExecRecord{}, err
		case !errors.Is(err, ErrGuestUnreachable):
			return ExecRecord{}, guestFailure(err, "read exec output")
		}
		unreached = err
	case StateStopped, StateFaulted:
	default:
		return ExecRecord{}, fail(ErrPreconditionFailed, "Machine %q is %s; read exec output needs Running or Stopped", id, state)
	}

	record, found, err := s.entrypointFromLog(ctx, id)
	switch {
	case err != nil && unreached != nil:
		return ExecRecord{}, guestFailure(unreached, "read exec output")
	case err != nil:
		return ExecRecord{}, fail(ErrFailed, "read the console log: %v", err)
	case found:
		return record, nil
	case unreached != nil:
		return ExecRecord{}, guestFailure(unreached, "read exec output")
	}
	return ExecRecord{ID: entrypointExec, Status: ExecCutShort,
		Reason: "the Machine ended and its console log has no line for the entrypoint's end"}, nil
}
