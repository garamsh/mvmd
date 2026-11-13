package machine

import (
	"context"
	"errors"
	"reflect"
	"sync"
)

// execLedger is the exec entries mvmd keeps, by identifier, and their State
// Store copy. It records an entry, cuts it short, drops it, restores it and
// settles it on read; the guest holds the output. An entry is replaced and
// never changed, so one read under mu is whole.
//
// A change is saved under mu, so the store sees the changes of one entry in the
// order memory does, and the entries a Machine's end cuts short are the ones
// saved with its record.
type execLedger struct {
	store StateStore
	guest Guest

	mu      sync.Mutex
	entries map[ExecID]*ExecEntry
}

func newExecLedger(store StateStore, guest Guest) *execLedger {
	return &execLedger{store: store, guest: guest, entries: make(map[ExecID]*ExecEntry)}
}

// load takes the entries the State Store holds, after a restart.
func (l *execLedger) load(entries []ExecEntry) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range entries {
		next := e
		l.entries[e.ID] = &next
	}
}

// drop forgets the entries of a Machine that is deleted, which frees their
// identifiers.
func (l *execLedger) drop(id MachineID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for exec, e := range l.entries {
		if e.Machine == id {
			delete(l.entries, exec)
		}
	}
}

// start records the call of an exec and asks the guest to run it. A repeat of
// a call the guest answered returns the record it was given. running refuses
// the call unless the Machine is Running, and is asked after a repeat is
// recognised and before anything is recorded.
func (l *execLedger) start(ctx context.Context, id MachineID, exec ExecID, request RequestID, spec ExecSpec, running func() error) (ExecRecord, error) {
	l.mu.Lock()
	stored, ok := l.entries[exec]
	var held ExecEntry
	if ok {
		held = *stored
	}
	l.mu.Unlock()
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
	if err := running(); err != nil {
		return ExecRecord{}, err
	}

	if !ok {
		// The identity of the call is kept before the guest is asked, so a
		// conflicting repeat is refused after a restart, and a command that may
		// have started is never forgotten. An outcome is remembered only when
		// the guest gave one.
		held = ExecEntry{Machine: id, ID: exec, Request: request, Spec: spec}
		if err := l.store.SaveExec(ctx, held); err != nil {
			return ExecRecord{}, fail(ErrFailed, "record exec %q, which was not started: %v", exec, err)
		}
		l.mu.Lock()
		l.entries[exec] = &held
		l.mu.Unlock()
	}

	started, err := l.guest.Exec(ctx, id, exec, spec)
	if err != nil {
		return ExecRecord{}, guestFailure(err, "start the command")
	}
	started.ID = exec
	answered := held
	answered.Record, answered.Answered = started, true
	if err := l.store.SaveExec(ctx, answered); err != nil {
		// The entry stays unanswered in memory as it is in the store, so a repeat
		// asks the guest again, which answers with the record it has.
		return ExecRecord{}, fail(ErrFailed, "the command started and its record could not be kept: %v", err)
	}
	l.mu.Lock()
	l.entries[exec] = &answered
	l.mu.Unlock()
	return started, nil
}

// recordOf is the record mvmd holds of the exec of the Machine: ErrNotFound for
// one it has no entry of, and a failure for one the State Store could not
// decode. An entry the guest never answered is taken to run until the guest
// says otherwise.
func (l *execLedger) recordOf(id MachineID, exec ExecID) (ExecRecord, error) {
	entry, err := l.entryOf(id, exec)
	if err != nil {
		return ExecRecord{}, err
	}
	if !entry.Answered {
		return ExecRecord{ID: exec, Status: ExecStarted}, nil
	}
	return entry.Record, nil
}

// known refuses an exec the Machine has no readable entry of.
func (l *execLedger) known(id MachineID, exec ExecID) error {
	_, err := l.entryOf(id, exec)
	return err
}

func (l *execLedger) entryOf(id MachineID, exec ExecID) (ExecEntry, error) {
	l.mu.Lock()
	entry, ok := l.entries[exec]
	var held ExecEntry
	if ok {
		held = *entry
	}
	l.mu.Unlock()
	switch {
	case !ok || held.Machine != id:
		return ExecEntry{}, fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
	case held.Unreadable != "":
		return ExecEntry{}, unreadableExec(held)
	}
	return held, nil
}

// unreadableExec is the answer to a call on an exec entry the State Store
// could not decode: whether its command ran is not known, so nothing is run or
// answered under it.
func unreadableExec(e ExecEntry) *Error {
	return fail(ErrFailed, "the record of exec %q could not be read: %s", e.ID, e.Unreadable)
}

// read answers a read of the output of an exec of the Machine that has an
// entry. running refuses the call unless the Machine is Running; a stopped
// Machine's guest cannot be asked, so a record that is final is all there is,
// and is answered without the guest, as is one that is cut short or never
// started. An end the guest reports is kept.
func (l *execLedger) read(ctx context.Context, id MachineID, exec ExecID, after, tail uint64, send func(OutputChunk) error, running func() error) (ExecRecord, error) {
	held, err := l.recordOf(id, exec)
	if err != nil {
		return ExecRecord{}, err
	}
	if err := running(); err != nil {
		if held.Status != ExecStarted {
			return held, nil
		}
		return ExecRecord{}, err
	}
	if held.Status == ExecNotStarted || held.Status == ExecCutShort {
		return held, nil
	}

	record, _, err := l.readGuest(ctx, id, exec, after, tail, send)
	if err != nil {
		return ExecRecord{}, err
	}
	if record.Status != ExecExited {
		return record, nil
	}
	return l.settle(ctx, id, exec, record)
}

// readGuest asks the guest for the output of an exec and its record. A failure
// is returned as the outcome the call gives; unreached says the guest could not
// be reached, which the entrypoint's read answers from the console instead.
func (l *execLedger) readGuest(ctx context.Context, id MachineID, exec ExecID, after, tail uint64, send func(OutputChunk) error) (record ExecRecord, unreached bool, err error) {
	record, err = l.guest.ReadExecOutput(ctx, id, exec, after, tail, send)
	if err == nil {
		record.ID = exec
		return record, false, nil
	}
	return ExecRecord{}, errors.Is(err, ErrGuestUnreachable), guestFailure(err, "read exec output")
}

// settle keeps the end the guest reported for the exec, unless the entry has
// been cut short, and returns the record the entry then holds. The save is
// made under mu, so the store sees the entry's changes in the order memory
// does, and a cut-short entry is not overwritten.
func (l *execLedger) settle(ctx context.Context, id MachineID, exec ExecID, record ExecRecord) (ExecRecord, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	current, ok := l.entries[exec]
	if !ok {
		return ExecRecord{}, fail(ErrNotFound, "no exec %q on Machine %q", exec, id)
	}
	if !current.Answered || current.Record.Status == ExecStarted {
		final := *current
		final.Record, final.Answered = record, true
		if err := l.store.SaveExec(ctx, final); err != nil {
			return ExecRecord{}, fail(ErrFailed, "the command ended and its record could not be kept: %v", err)
		}
		l.entries[exec] = &final
	}
	return l.entries[exec].Record, nil
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
// as they become. The caller holds mu.
func (l *execLedger) cutShortOf(id MachineID, why string) []ExecEntry {
	var cut []ExecEntry
	for _, e := range l.entries {
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
// memory. save has recorded them with the state that cut them.
func (l *execLedger) cutShort(id MachineID, why string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, e := range l.cutShortOf(id, why) {
		next := e
		l.entries[e.ID] = &next
	}
}

// save records the record, adding the Machine's cut-short commands when its
// run ends. It holds mu, so no entry changes between the choice of the entries
// and their being saved.
func (l *execLedger) save(ctx context.Context, record Record, ends bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if ends {
		record.Execs = l.cutShortOf(record.ID, endedWith(record.Machine.State))
	}
	return l.store.Save(ctx, record)
}
