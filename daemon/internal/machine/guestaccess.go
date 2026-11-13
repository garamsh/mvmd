package machine

import (
	"context"
	"errors"
	"io"
	"strings"
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
	m := *rec.Machine
	s.mu.Unlock()
	if err := m.requireAgent(call); err != nil {
		return err
	}
	if state := m.State; state != StateRunning {
		return fail(ErrPreconditionFailed, "Machine %q is %s; %s needs Running", id, state, call)
	}
	return nil
}

// requireGuestPath refuses a guest file path that is empty or not absolute, as
// the Guest Agent does, before the Guest is called.
func requireGuestPath(path string) error {
	switch {
	case path == "":
		return fail(ErrInvalid, "the path is empty")
	case !strings.HasPrefix(path, "/"):
		return fail(ErrInvalid, "the path %q is not absolute", path)
	}
	return nil
}

// guestFailure maps what a Guest returned to an outcome.
func guestFailure(err error, call string) *Error {
	switch {
	case errors.Is(err, ErrFileAbsent), errors.Is(err, ErrExecAbsent):
		return fail(ErrNotFound, "%s: %v", call, err)
	case errors.Is(err, ErrNotRegular), errors.Is(err, ErrNotDirectory), errors.Is(err, ErrSignalInvalid), errors.Is(err, ErrPatternInvalid):
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
	return s.execs.start(ctx, id, exec, request, spec, func() error { return s.requireRunning(id, "exec") })
}

func (s *service) ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after, tail uint64, send func(OutputChunk) error) (ExecRecord, error) {
	if exec == entrypointExec {
		return s.readEntrypoint(ctx, id, after, tail, send)
	}
	return s.execs.read(ctx, id, exec, after, tail, send, func() error { return s.requireRunning(id, "read exec output") })
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
	if exec == entrypointExec {
		// The guest holds the entrypoint in its exec table and signals it as any
		// command (guest.md); mvmd keeps no record of it.
		if err := s.guest.Signal(ctx, id, exec, signal); err != nil {
			return guestFailure(err, "signal exec")
		}
		return nil
	}
	if err := s.execs.known(id, exec); err != nil {
		return err
	}
	if err := s.guest.Signal(ctx, id, exec, signal); err != nil {
		return guestFailure(err, "signal exec")
	}
	return nil
}

func (s *service) ReadFile(ctx context.Context, id MachineID, path string, send func(chunk []byte) error) error {
	if err := requireGuestPath(path); err != nil {
		return err
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
	if err := requireGuestPath(path); err != nil {
		return err
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
	if err := requireGuestPath(path); err != nil {
		return err
	}
	if err := s.requireRunning(id, "patch file"); err != nil {
		return err
	}
	if _, err := s.guest.PatchFile(ctx, id, path, baseDigest, expectedDigest, patch); err != nil {
		return guestFailure(err, "patch file")
	}
	return nil
}

func (s *service) ListFiles(ctx context.Context, id MachineID, path string, send func(entries []FileEntry) error) error {
	if err := requireGuestPath(path); err != nil {
		return err
	}
	if err := s.requireRunning(id, "list files"); err != nil {
		return err
	}
	if err := s.guest.ListFiles(ctx, id, path, send); err != nil {
		return guestFailure(err, "list files")
	}
	return nil
}

func (s *service) SearchFiles(ctx context.Context, id MachineID, path, pattern string, send func(paths []string) error) error {
	if err := requireGuestPath(path); err != nil {
		return err
	}
	if err := s.requireRunning(id, "search files"); err != nil {
		return err
	}
	if err := s.guest.SearchFiles(ctx, id, path, pattern, send); err != nil {
		return guestFailure(err, "search files")
	}
	return nil
}
