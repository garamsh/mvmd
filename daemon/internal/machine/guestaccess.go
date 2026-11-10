package machine

import (
	"context"
	"errors"
	"io"
	"reflect"
)

// entrypointExec is the exec identifier the Guest Agent keeps for the
// entrypoint (guest.md); a client's Exec under it would run nothing.
const entrypointExec ExecID = "entrypoint"

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
	case errors.Is(err, ErrNotRegular), errors.Is(err, ErrSignalInvalid):
		return fail(ErrInvalid, "%s: %v", call, err)
	case errors.Is(err, ErrPatchPrecondition), errors.Is(err, ErrExecNotRunning):
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
	if exec == entrypointExec {
		return ExecRecord{}, fail(ErrInvalid, "the exec identifier %q belongs to the guest's entrypoint", exec)
	}
	defer s.lock(id)()

	s.execMu.Lock()
	stored, ok := s.execs[exec]
	var held ExecEntry
	if ok {
		held = *stored
	}
	s.execMu.Unlock()
	if ok {
		if held.Unreadable != "" {
			return ExecRecord{}, unreadableExec(held)
		}
		if held.Machine != id || held.Request != request || !reflect.DeepEqual(held.Spec, spec) {
			return ExecRecord{}, fail(ErrConflict, "exec %q exists under another request ID or other parameters", exec)
		}
		if held.Answered {
			return held.Record, nil
		}
		// The guest never answered the first call, so the command may or may not
		// run: ask again. The guest returns the existing record of an exec ID it
		// has and runs nothing twice.
	}
	if err := s.requireRunning(id, "exec"); err != nil {
		return ExecRecord{}, err
	}

	if !ok {
		// The identity of the call is kept before the guest is asked, so a
		// conflicting repeat is refused after a restart, and a command that may
		// have started is never forgotten. An outcome is remembered only when
		// the guest gave one.
		held = ExecEntry{Machine: id, ID: exec, Request: request, Spec: spec}
		if err := s.store.SaveExec(ctx, held); err != nil {
			return ExecRecord{}, fail(ErrFailed, "record exec %q, which was not started: %v", exec, err)
		}
		s.execMu.Lock()
		s.execs[exec] = &held
		s.execMu.Unlock()
	}

	started, err := s.guest.Exec(ctx, id, exec, spec)
	if err != nil {
		return ExecRecord{}, guestFailure(err, "start the command")
	}
	started.ID = exec
	answered := held
	answered.Record, answered.Answered = started, true
	if err := s.store.SaveExec(ctx, answered); err != nil {
		// The entry stays unanswered in memory as it is in the store, so a repeat
		// asks the guest again, which answers with the record it has.
		return ExecRecord{}, fail(ErrFailed, "the command started and its record could not be kept: %v", err)
	}
	s.execMu.Lock()
	s.execs[exec] = &answered
	s.execMu.Unlock()
	return started, nil
}

// dropExecs forgets the records of a Machine that is deleted, which frees
// their identifiers.
func (s *service) dropExecs(id MachineID) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	for exec, e := range s.execs {
		if e.Machine == id {
			delete(s.execs, exec)
		}
	}
}

// unreadableExec is the answer to a call on an exec entry the State Store
// could not decode: whether its command ran is not known, so nothing is run or
// answered under it.
func unreadableExec(e ExecEntry) *Error {
	return fail(ErrFailed, "the record of exec %q could not be read: %s", e.ID, e.Unreadable)
}

// endedWith is the reason a state that ends the run gives for the commands it
// cuts short.
func endedWith(state State) string { return "the Machine is " + state.String() }

// cutShortEntry is the entry of a command whose Machine's VMM has ended, so
// nothing in it runs and the guest cannot be asked; changed is false for an
// entry that is already final.
func cutShortEntry(e ExecEntry, why string) (cut ExecEntry, changed bool) {
	switch {
	case e.Unreadable != "":
		// Nothing of it is known, and it is not rewritten.
	case !e.Answered:
		// The guest never answered the start, so whether the command ran is
		// not known; with its Machine gone it never will be.
		e.Record = ExecRecord{ID: e.ID, Status: ExecCutShort,
			Reason: "the guest never answered the start, so whether the command ran is not known; the Machine is gone: " + why}
		e.Answered = true
		return e, true
	case e.Record.Status == ExecStarted:
		e.Record.Status = ExecCutShort
		e.Record.Reason = "the command ended with its Machine: " + why
		return e, true
	}
	return e, false
}

// cutShortOf returns the entries of the Machine that a run's end cuts short,
// as they become. The caller holds execMu.
func (s *service) cutShortOf(id MachineID, why string) []ExecEntry {
	var cut []ExecEntry
	for _, e := range s.execs {
		if e.Machine != id {
			continue
		}
		if next, changed := cutShortEntry(*e, why); changed {
			cut = append(cut, next)
		}
	}
	return cut
}

// cutShort marks the commands of a Machine that still run as cut short in
// memory. commit has recorded them with the state that cut them.
func (s *service) cutShort(id MachineID, why string) {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	for _, e := range s.cutShortOf(id, why) {
		next := e
		s.execs[e.ID] = &next
	}
}

func (s *service) ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after uint64, send func(OutputChunk) error) (ExecRecord, error) {
	if exec == entrypointExec {
		return s.readEntrypoint(ctx, id, after, send)
	}
	s.execMu.Lock()
	entry, ok := s.execs[exec]
	var held ExecRecord
	if ok {
		held = entry.Record
		if !entry.Answered {
			// The guest never answered the start, so the command is taken to run
			// until the guest says otherwise.
			held = ExecRecord{ID: exec, Status: ExecStarted}
		}
	}
	s.execMu.Unlock()
	if !ok || entry.Machine != id {
		return ExecRecord{}, fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
	}
	if entry.Unreadable != "" {
		return ExecRecord{}, unreadableExec(*entry)
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
		// The save is made under execMu, so the store sees the entry's changes in
		// the order memory does, and a cut-short entry is not overwritten.
		s.execMu.Lock()
		defer s.execMu.Unlock()
		current, ok := s.execs[exec]
		if !ok {
			return ExecRecord{}, fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
		}
		if !current.Answered || current.Record.Status == ExecStarted {
			final := *current
			final.Record, final.Answered = record, true
			if err := s.store.SaveExec(ctx, final); err != nil {
				return ExecRecord{}, fail(ErrFailed, "the command ended and its record could not be kept: %v", err)
			}
			s.execs[exec] = &final
		}
		return s.execs[exec].Record, nil
	}
	return record, nil
}

func (s *service) SignalExec(ctx context.Context, id MachineID, exec ExecID, signal int32) error {
	switch {
	case exec == "":
		return fail(ErrInvalid, "the exec identifier is empty")
	case signal < 1:
		return fail(ErrInvalid, "%d is not a signal number", signal)
	}
	if err := s.requireRunning(id, "signal exec"); err != nil {
		return err
	}
	s.execMu.Lock()
	entry, ok := s.execs[exec]
	var held ExecEntry
	if ok {
		held = *entry
	}
	s.execMu.Unlock()
	if !ok || held.Machine != id {
		return fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
	}
	if held.Unreadable != "" {
		return unreadableExec(held)
	}
	if err := s.guest.Signal(ctx, id, exec, signal); err != nil {
		return guestFailure(err, "signal exec")
	}
	return nil
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

func (s *service) ListFiles(ctx context.Context, id MachineID, path string) ([]FileEntry, error) {
	if path == "" {
		return nil, fail(ErrInvalid, "the path is empty")
	}
	if err := s.requireRunning(id, "list files"); err != nil {
		return nil, err
	}
	entries, err := s.guest.ListFiles(ctx, id, path)
	if err != nil {
		return nil, guestFailure(err, "list files")
	}
	return entries, nil
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
