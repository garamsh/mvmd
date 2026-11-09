package machine

import (
	"context"
	"io"
	"slices"
	"sync"
	"time"
)

// Service is what the Machine lifecycle offers a driving adapter. Every call
// returns nil on success, or an *Error whose kind is one of the Err values.
// Calls on one Machine run one at a time; calls on different Machines run
// together, and share only short critical sections.
type Service interface {
	// Restore rebuilds the lifecycle from the State Store after a restart and
	// reconciles its Machines against the live supervisors. The composition
	// site calls it once, before the fencing Reconcile and before any other
	// call.
	Restore(ctx context.Context) error
	// Create configures a Machine without booting it.
	Create(ctx context.Context, id MachineID, request RequestID, spec Spec) error
	Start(ctx context.Context, id MachineID) error
	// Stop asks the guest to stop, waits up to grace, then forces the VMM down.
	Stop(ctx context.Context, id MachineID, grace time.Duration) error
	// ForceStop ends the Machine's VMM at once, with no request to the guest and
	// no grace period, as Stop does otherwise. The fencing expiry uses it.
	ForceStop(ctx context.Context, id MachineID) error
	Pause(ctx context.Context, id MachineID) error
	Resume(ctx context.Context, id MachineID) error
	Delete(ctx context.Context, id MachineID) error
	Inspect(ctx context.Context, id MachineID) (Machine, error)
	// List returns every Machine on the node, ordered by identifier.
	List(ctx context.Context) ([]Machine, error)
	AttachDisk(ctx context.Context, id MachineID, attachment DiskAttachment) error
	DetachDisk(ctx context.Context, id MachineID, disk DiskID) error
	SetResources(ctx context.Context, id MachineID, target Resources) error

	// The guest access calls need the Machine Running and fail with
	// ErrPreconditionFailed otherwise.
	//
	// Exec starts a command under the exec identifier, at most once per request
	// ID, and returns its record.
	Exec(ctx context.Context, id MachineID, exec ExecID, request RequestID, spec ExecSpec) (ExecRecord, error)
	// ReadExecOutput sends the output after the offset, and returns the exec's
	// record, whose Status is ExecStarted while the command runs. An offset the
	// guest no longer holds is an *OutputGoneError. The exec "entrypoint" is the
	// guest's own: Running it is asked of the guest; Stopped, Faulted, or
	// Running with a guest that no longer answers, its record is what the
	// Guest Agent wrote to the console log, and no output is sent.
	ReadExecOutput(ctx context.Context, id MachineID, exec ExecID, after uint64, send func(OutputChunk) error) (ExecRecord, error)
	ReadFile(ctx context.Context, id MachineID, path string, send func(chunk []byte) error) error
	// WriteFile commits the whole content or leaves the old.
	WriteFile(ctx context.Context, id MachineID, path string, content io.Reader) error
	PatchFile(ctx context.Context, id MachineID, path, baseDigest, expectedDigest string, patch []byte) error
	SearchFiles(ctx context.Context, id MachineID, path, pattern string) ([]string, error)

	// Console opens a session to the console of a Running or Paused Machine,
	// otherwise ErrPreconditionFailed. What the guest writes to its console is
	// read from the session and what is written to it is the console's input.
	// Any number of sessions may be open at once and none changes the Machine.
	// The caller closes the session, and its context ending closes it too.
	Console(ctx context.Context, id MachineID) (io.ReadWriteCloser, error)

	// Snapshot holds the Running or Paused Machine's guest paused while the
	// Driver and the Snapshot Store take it, and returns it to its state.
	Snapshot(ctx context.Context, id MachineID, snapshot SnapshotID, request RequestID) error
	// RestoreSnapshot creates the Machine id, Running, from the snapshot, with
	// the policy as its network policy. It is gated as Start is.
	RestoreSnapshot(ctx context.Context, id MachineID, request RequestID, snapshot SnapshotID, policy NetworkPolicy) error
}

// machineLock is one Machine's lock and the number of calls using it, which
// mu guards.
type machineLock struct {
	sync.Mutex
	users int
}

// snapshotLock is one snapshot's lock and the number of calls using it, which
// mu guards. Snapshot holds it exclusively; a restore holds it shared.
type snapshotLock struct {
	sync.RWMutex
	users int
}

type service struct {
	driver     Driver
	supervisor Supervision
	network    Network
	disks      Disk
	images     ImageStore
	snapshots  SnapshotStore
	guest      Guest
	clock      WallClock
	store      StateStore
	permitter  StartPermitter
	uidRange   UIDRange
	events     Events
	// bootTimeout bounds the wait for the guest's boot in Start.
	bootTimeout time.Duration
	// callTimeout bounds an accepted call; callContext applies it, detaching
	// the call from its caller. A test replaces callContext to run a call on the
	// caller's own context.
	callTimeout time.Duration
	callContext func(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc)
	// compensate is callContext's counterpart for compensating.
	compensate func(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc)

	// mu guards the maps below and every record's Machine pointer, and is held
	// only for map and pointer work, never across a port call. A Machine's
	// value is replaced and never changed, so a reader holding mu sees a whole
	// one.
	mu      sync.Mutex
	records map[MachineID]*Record
	locks   map[MachineID]*machineLock
	// execs are the exec entries mvmd keeps, by identifier. The State Store
	// holds the same, and Restore reads them back; the guest holds the output.
	// An entry is replaced and never changed, so one read under execMu is whole.
	// A change is saved under execMu, so the store sees them in the order
	// memory does.
	execMu sync.Mutex
	execs  map[ExecID]*ExecEntry
	// snapLocks serializes Snapshot against the restores of the same snapshot.
	// A call takes a Machine's lock before a snapshot's.
	snapLocks map[SnapshotID]*snapshotLock
	// held maps each disk to its one holder (ADR 0009).
	held         map[DiskID]MachineID
	usedUID      map[MachineID]uint32 // allocated UID per Machine
	usedUIDByUID map[uint32]MachineID // reverse index for O(1) collision check
}

// DefaultBootTimeout is how long Start waits for the guest's boot when the
// node names no other bound.
const DefaultBootTimeout = 2 * time.Minute

// DefaultCallTimeout is how long an accepted call may run, from the moment it
// holds its Machine's lock, when the node names no other bound. A Stop has its
// grace period on top.
const DefaultCallTimeout = 10 * time.Minute

// WithCallTimeout bounds how long an accepted state-changing call runs,
// whatever the caller's context says. A bound that is not positive is ignored.
func WithCallTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.callTimeout = d
		}
	}
}

// accepted returns the context the steps of an accepted state-changing call
// run on, and its release. The caller's disconnect or deadline does not end it
// (runtime-api.md §Calling rules): a call completes or fails into the state its
// table row names. The lifecycle's own bound does end it, so a step that hangs
// cannot hold the Machine's lock forever; extra is added to the bound for a
// wait the caller asked for. It is the shape bootGuest uses for the boot.
func (s *service) accepted(ctx context.Context, extra time.Duration) (context.Context, context.CancelFunc) {
	return s.callContext(ctx, s.callTimeout+extra)
}

// compensationTimeout bounds the steps that put right what a failed call did,
// so they can run when the call's own bound has passed. It is short because
// each is one release or removal.
const compensationTimeout = 30 * time.Second

// compensating returns the context for the steps that undo what a call did
// before it failed: its resources released, its failure recorded, its guest
// resumed. It is the call's values on a context of its own, detached from the
// call's, so a call that the bound or the caller ended still reaches the state
// its row names.
func (s *service) compensating(ctx context.Context) (context.Context, context.CancelFunc) {
	return s.compensate(ctx, compensationTimeout)
}

// detached is a context that carries ctx's values but not its cancellation,
// ended after bound.
func detached(ctx context.Context, bound time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), bound)
}

// Option changes how the lifecycle behaves from its defaults.
type Option func(*service)

// WithBootTimeout bounds how long Start waits for the guest to finish booting,
// whatever the caller's context says. A bound that is not positive is ignored.
func WithBootTimeout(d time.Duration) Option {
	return func(s *service) {
		if d > 0 {
			s.bootTimeout = d
		}
	}
}

// NewService returns the lifecycle over the given ports. uidRange names the
// range of UIDs the node reserves for its Machines (ADR 0015). Every state
// change and every event the Driver raises goes to events.
func NewService(driver Driver, supervisor Supervision, network Network, disks Disk, images ImageStore, snapshots SnapshotStore, guest Guest, clock WallClock, store StateStore, permitter StartPermitter, uidRange UIDRange, events Events, options ...Option) Service {
	driver.Observe(events.Reported)
	s := &service{
		bootTimeout:  DefaultBootTimeout,
		callTimeout:  DefaultCallTimeout,
		callContext:  detached,
		compensate:   detached,
		driver:       driver,
		supervisor:   supervisor,
		network:      network,
		disks:        disks,
		images:       images,
		snapshots:    snapshots,
		guest:        guest,
		clock:        clock,
		execs:        make(map[ExecID]*ExecEntry),
		store:        store,
		permitter:    permitter,
		uidRange:     uidRange,
		events:       events,
		records:      make(map[MachineID]*Record),
		locks:        make(map[MachineID]*machineLock),
		snapLocks:    make(map[SnapshotID]*snapshotLock),
		held:         make(map[DiskID]MachineID),
		usedUID:      make(map[MachineID]uint32),
		usedUIDByUID: make(map[uint32]MachineID),
	}
	for _, o := range options {
		o(s)
	}
	return s
}

// lock serializes the calls on one Machine and returns the unlock. The entry
// exists while a call holds or waits for it and is removed by the last one,
// so the map holds only the identifiers being called now.
func (s *service) lock(id MachineID) func() {
	s.mu.Lock()
	l, ok := s.locks[id]
	if !ok {
		l = new(machineLock)
		s.locks[id] = l
	}
	l.users++
	s.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(s.locks, id)
		}
	}
}

// lockSnapshot takes the snapshot's lock, exclusively or shared, and returns
// the unlock. The entry exists while a call holds or waits for it.
func (s *service) lockSnapshot(id SnapshotID, exclusive bool) func() {
	s.mu.Lock()
	l, ok := s.snapLocks[id]
	if !ok {
		l = new(snapshotLock)
		s.snapLocks[id] = l
	}
	l.users++
	s.mu.Unlock()
	lock, unlock := l.RLock, l.RUnlock
	if exclusive {
		lock, unlock = l.Lock, l.Unlock
	}
	lock()
	return func() {
		unlock()
		s.mu.Lock()
		defer s.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(s.snapLocks, id)
		}
	}
}

func (s *service) lookup(id MachineID) (*Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	return rec, ok
}

// machine returns the record of an existing Machine.
func (s *service) machine(id MachineID) (*Record, error) {
	rec, ok := s.lookup(id)
	if !ok || rec.Machine == nil {
		return nil, fail(ErrNotFound, "no Machine %q", id)
	}
	return rec, nil
}

// setMachine makes m the record's Machine, the one place a state is committed,
// and reports a changed state, or a first one, to Events. A call holds the
// Machine's lock here, so the events of one Machine come in the order its
// states were committed.
func (s *service) setMachine(rec *Record, m *Machine) {
	s.mu.Lock()
	changed := rec.Machine == nil || rec.Machine.State != m.State
	rec.Machine = m
	s.mu.Unlock()
	if changed {
		if m.State == StateStopped || m.State == StateFaulted {
			// The VMM has ended or cannot be vouched for: what ran in it is cut short.
			s.cutShort(rec.ID, endedWith(m.State))
		}
		s.events.StateChanged(rec.ID, m.State)
	}
}

// commit durably records next and, only then, makes it the Machine's state. A
// state that ends the run is recorded with the commands it cuts short, in one
// step.
func (s *service) commit(ctx context.Context, rec *Record, next Machine) error {
	next = next.clone()
	s.mu.Lock()
	ends := (rec.Machine == nil || rec.Machine.State != next.State) && (next.State == StateStopped || next.State == StateFaulted)
	s.mu.Unlock()
	if err := s.save(ctx, Record{ID: rec.ID, Machine: &next, Request: rec.Request}, ends); err != nil {
		return err
	}
	s.setMachine(rec, &next)
	return nil
}

// save records the record, adding the Machine's cut-short commands when its
// run ends. It holds execMu, so no exec entry changes between the choice of
// the entries and their being saved.
func (s *service) save(ctx context.Context, record Record, ends bool) error {
	s.execMu.Lock()
	defer s.execMu.Unlock()
	if ends {
		record.Execs = s.cutShortOf(record.ID, endedWith(record.Machine.State))
	}
	return s.store.Save(ctx, record)
}

// unrecorded handles an effect that happened but whose state could not be
// recorded: mvmd cannot establish the Machine's state, so it is Faulted.
func (s *service) unrecorded(rec *Record, err error) error {
	faulted := rec.Machine.clone()
	faulted.State = StateFaulted
	faulted.FailureReason = "the Machine's state could not be recorded: " + err.Error()
	s.setMachine(rec, &faulted)
	return fail(ErrFailed, "%s", faulted.FailureReason)
}

func (s *service) requireNotFaulted(m Machine, call string) error {
	if m.State == StateFaulted {
		return fail(ErrPreconditionFailed, "Machine %q is Faulted; %s does not apply, only stop and delete do", m.ID, call)
	}
	return nil
}

// claim makes id the holder of the disks, all or none, and reports the first
// disk another holder has, with that holder. A disk its claimant already holds
// is a conflict too: the caller decides what a repeat means.
func (s *service) claim(id MachineID, disks ...DiskID) (taken DiskID, holder MachineID, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range disks {
		if h, held := s.held[d]; held {
			return d, h, false
		}
	}
	for _, d := range disks {
		s.held[d] = id
	}
	return "", "", true
}

func (s *service) release(disks ...DiskID) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range disks {
		delete(s.held, d)
	}
}

// allocateUID allocates the lowest free UID in the range for the given
// Machine. It must be called under mu.
func (s *service) allocateUID(id MachineID) (uint32, error) {
	for i := uint32(0); i < s.uidRange.Count; i++ {
		u := s.uidRange.First + i
		if _, occupied := s.usedUIDByUID[u]; !occupied {
			s.usedUID[id] = u
			s.usedUIDByUID[u] = id
			return u, nil
		}
	}
	return 0, fail(ErrPreconditionFailed, "the UID range is exhausted")
}

// releaseUID returns the UID of the given Machine to the range.
// The caller must hold mu.
func (s *service) releaseUID(id MachineID) {
	uid, ok := s.usedUID[id]
	if ok {
		delete(s.usedUID, id)
		delete(s.usedUIDByUID, uid)
	}
}

func (s *service) Inspect(ctx context.Context, id MachineID) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.records[id]
	if !ok || rec.Machine == nil {
		return Machine{}, fail(ErrNotFound, "no Machine %q", id)
	}
	m := rec.Machine.clone()
	m.Driver = s.driver.Info()
	return m, nil
}

func (s *service) List(ctx context.Context) ([]Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Machine
	for _, rec := range s.records {
		if rec.Machine != nil {
			m := rec.Machine.clone()
			m.Driver = s.driver.Info()
			out = append(out, m)
		}
	}
	slices.SortFunc(out, func(a, b Machine) int {
		switch {
		case a.ID < b.ID:
			return -1
		case a.ID > b.ID:
			return 1
		}
		return 0
	})
	return out, nil
}
