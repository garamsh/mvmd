package machine

import (
	"context"
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

type service struct {
	driver     Driver
	supervisor Supervision
	network    Network
	disks      Disk
	images     ImageStore
	snapshots  SnapshotStore
	store      StateStore
	permitter  StartPermitter
	uidRange   UIDRange
	events     Events

	// mu guards the maps below and every record's Machine pointer, and is held
	// only for map and pointer work, never across a port call. A Machine's
	// value is replaced and never changed, so a reader holding mu sees a whole
	// one.
	mu      sync.Mutex
	records map[MachineID]*Record
	locks   map[MachineID]*machineLock
	// held maps each disk to its one holder (ADR 0009).
	held         map[DiskID]MachineID
	usedUID      map[MachineID]uint32 // allocated UID per Machine
	usedUIDByUID map[uint32]MachineID // reverse index for O(1) collision check
}

// NewService returns the lifecycle over the given ports. uidRange names the
// range of UIDs the node reserves for its Machines (ADR 0015). Every state
// change and every event the Driver raises goes to events.
func NewService(driver Driver, supervisor Supervision, network Network, disks Disk, images ImageStore, snapshots SnapshotStore, store StateStore, permitter StartPermitter, uidRange UIDRange, events Events) Service {
	driver.Observe(events.Reported)
	return &service{
		driver:       driver,
		supervisor:   supervisor,
		network:      network,
		disks:        disks,
		images:       images,
		snapshots:    snapshots,
		store:        store,
		permitter:    permitter,
		uidRange:     uidRange,
		events:       events,
		records:      make(map[MachineID]*Record),
		locks:        make(map[MachineID]*machineLock),
		held:         make(map[DiskID]MachineID),
		usedUID:      make(map[MachineID]uint32),
		usedUIDByUID: make(map[uint32]MachineID),
	}
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
		s.events.StateChanged(rec.ID, m.State)
	}
}

// commit durably records next and, only then, makes it the Machine's state.
func (s *service) commit(ctx context.Context, rec *Record, next Machine) error {
	next = next.clone()
	if err := s.store.Save(ctx, Record{ID: rec.ID, Machine: &next, Request: rec.Request}); err != nil {
		return err
	}
	s.setMachine(rec, &next)
	return nil
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
