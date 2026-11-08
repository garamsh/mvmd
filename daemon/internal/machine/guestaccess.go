package machine

import (
	"context"
	"errors"
	"io"
	"reflect"
)

// execEntry is what mvmd keeps of one exec: the call that made it, for its
// repeat, and the record as far as mvmd knows it.
type execEntry struct {
	machine MachineID
	request RequestID
	spec    ExecSpec
	record  ExecRecord
	// failure is the outcome of an Exec that did not start, for its repeat.
	failure *Error
}

// requireRunning refuses a guest access call unless the Machine exists and is
// Running.
func (s *service) requireRunning(id MachineID, call string) error {
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	state := rec.Machine.State
	s.mu.Unlock()
	if state != StateRunning {
		return fail(ErrPreconditionFailed, "Machine %q is %s; %s needs Running", id, state, call)
	}
	return nil
}

// guestFailure maps what a Guest returned to an outcome.
func guestFailure(err error, call string) *Error {
	switch {
	case errors.Is(err, ErrFileAbsent), errors.Is(err, ErrExecAbsent):
		return fail(ErrNotFound, "%s: %v", call, err)
	case errors.Is(err, ErrNotRegular):
		return fail(ErrInvalid, "%s: %v", call, err)
	case errors.Is(err, ErrPatchPrecondition):
		return fail(ErrPreconditionFailed, "%s: %v", call, err)
	default:
		return fail(ErrFailed, "%s: %v", call, err)
	}
}

func (s *service) Exec(ctx context.Context, id MachineID, exec ExecID, request RequestID, spec ExecSpec) (ExecRecord, error) {
	switch {
	case exec == "":
		return ExecRecord{}, fail(ErrInvalid, "the exec identifier is empty")
	case request == "":
		return ExecRecord{}, fail(ErrInvalid, "the request ID is empty")
	case len(spec.Command) == 0:
		return ExecRecord{}, fail(ErrInvalid, "the command is empty")
	}
	defer s.lock(id)()

	s.execMu.Lock()
	entry, ok := s.execs[exec]
	s.execMu.Unlock()
	if ok {
		if entry.machine == id && entry.request == request && reflect.DeepEqual(entry.spec, spec) {
			if entry.failure != nil {
				return entry.record, entry.failure
			}
			return entry.record, nil
		}
		return ExecRecord{}, fail(ErrConflict, "exec %q exists under another request ID or other parameters", exec)
	}
	if err := s.requireRunning(id, "exec"); err != nil {
		return ExecRecord{}, err
	}

	started, err := s.guest.Exec(ctx, id, exec, spec)
	entry = &execEntry{machine: id, request: request, spec: spec, record: started}
	entry.record.ID = exec
	var failure *Error
	if err != nil {
		failure = guestFailure(err, "start the command")
		entry.record = ExecRecord{ID: exec, Status: ExecNotStarted, Reason: failure.Reason}
		entry.failure = failure
	}
	s.execMu.Lock()
	s.execs[exec] = entry
	s.execMu.Unlock()
	if failure != nil {
		return entry.record, failure
	}
	return entry.record, nil
}

// cutShort marks the commands of a Machine that still run as cut short: its
// VMM has ended, so nothing in it runs and the guest cannot be asked.
func (s *service) cutShort(id MachineID, why string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	for _, e := range s.execs {
		if e.machine == id && e.record.Status == ExecStarted {
			e.record.Status = ExecCutShort
			e.record.Reason = "the command ended with its Machine: " + why
		}
	}
}

func (s *service) ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after uint64, send func(OutputChunk) error) (ExecRecord, error) {
	s.execMu.Lock()
	entry, ok := s.execs[exec]
	var held ExecRecord
	if ok {
		held = entry.record
	}
	s.execMu.Unlock()
	if !ok || entry.machine != id {
		return ExecRecord{}, fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
	}
	if err := s.requireRunning(id, "read exec output"); err != nil {
		// A stopped Machine's guest cannot be asked: the record is all there is.
		if held.Status != ExecStarted {
			return held, nil
		}
		return ExecRecord{}, err
	}
	if held.Status == ExecNotStarted || held.Status == ExecCutShort {
		return held, nil
	}

	record, err := s.guest.ReadExecOutput(ctx, id, exec, after, send)
	var gone *OutputGoneError
	if errors.As(err, &gone) {
		return ExecRecord{}, err
	}
	if err != nil {
		return ExecRecord{}, guestFailure(err, "read exec output")
	}
	record.ID = exec
	if record.Status == ExecExited {
		s.execMu.Lock()
		if entry.record.Status == ExecStarted {
			entry.record = record
		}
		held = entry.record
		s.execMu.Unlock()
		return held, nil
	}
	return record, nil
}

func (s *service) ReadFile(ctx context.Context, id MachineID, path string, send func(chunk []byte) error) error {
	if path == "" {
		return fail(ErrInvalid, "the path is empty")
	}
	if err := s.requireRunning(id, "read file"); err != nil {
		return err
	}
	if err := s.guest.ReadFile(ctx, id, path, send); err != nil {
		return guestFailure(err, "read file")
	}
	return nil
}

func (s *service) WriteFile(ctx context.Context, id MachineID, path string, content io.Reader) error {
	if path == "" {
		return fail(ErrInvalid, "the path is empty")
	}
	if err := s.requireRunning(id, "write file"); err != nil {
		return err
	}
	if err := s.guest.WriteFile(ctx, id, path, content); err != nil {
		return guestFailure(err, "write file")
	}
	return nil
}

func (s *service) PatchFile(ctx context.Context, id MachineID, path, baseDigest, expectedDigest string, patch []byte) error {
	if path == "" {
		return fail(ErrInvalid, "the path is empty")
	}
	if err := s.requireRunning(id, "patch file"); err != nil {
		return err
	}
	if _, err := s.guest.PatchFile(ctx, id, path, baseDigest, expectedDigest, patch); err != nil {
		return guestFailure(err, "patch file")
	}
	return nil
}

func (s *service) SearchFiles(ctx context.Context, id MachineID, path, pattern string) ([]string, error) {
	if path == "" {
		return nil, fail(ErrInvalid, "the path is empty")
	}
	if err := s.requireRunning(id, "search files"); err != nil {
		return nil, err
	}
	matches, err := s.guest.SearchFiles(ctx, id, path, pattern)
	if err != nil {
		return nil, guestFailure(err, "search files")
	}
	return matches, nil
}
