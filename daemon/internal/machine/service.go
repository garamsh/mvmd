package machine

import (
	"context"
	"slices"
	"sync"
	"time"
)

// Service is what the Machine lifecycle offers a driving adapter. Every call
// returns nil on success, or an *Error whose kind is one of the Err values.
// Calls are serialized: one runs at a time.
type Service interface {
	// Create configures a Machine without booting it.
	Create(ctx context.Context, id MachineID, request RequestID, spec Spec) error
	Start(ctx context.Context, id MachineID) error
	// Stop asks the guest to stop, waits up to grace, then forces the VMM down.
	Stop(ctx context.Context, id MachineID, grace time.Duration) error
	Pause(ctx context.Context, id MachineID) error
	Resume(ctx context.Context, id MachineID) error
	Delete(ctx context.Context, id MachineID) error
	Inspect(ctx context.Context, id MachineID) (Machine, error)
	// List returns every Machine on the node, ordered by identifier.
	List(ctx context.Context) ([]Machine, error)
	AttachDisk(ctx context.Context, id MachineID, attachment DiskAttachment) error
	DetachDisk(ctx context.Context, id MachineID, disk DiskID) error
	SetResources(ctx context.Context, id MachineID, target Resources) error
}

type service struct {
	driver     Driver
	supervisor Supervision
	network    Network
	disks      Disk
	images     ImageStore
	store      StateStore
	permitter  StartPermitter

	mu      sync.Mutex
	records map[MachineID]*Record
}

// NewService returns the lifecycle over the given ports.
func NewService(driver Driver, supervisor Supervision, network Network, disks Disk, images ImageStore, store StateStore, permitter StartPermitter) Service {
	return &service{
		driver:     driver,
		supervisor: supervisor,
		network:    network,
		disks:      disks,
		images:     images,
		store:      store,
		permitter:  permitter,
		records:    make(map[MachineID]*Record),
	}
}

// machine returns the record of an existing Machine.
func (s *service) machine(id MachineID) (*Record, error) {
	rec, ok := s.records[id]
	if !ok || rec.Machine == nil {
		return nil, fail(ErrNotFound, "no Machine %q", id)
	}
	return rec, nil
}

// commit durably records next and, only then, makes it the Machine's state.
func (s *service) commit(ctx context.Context, rec *Record, next Machine) error {
	next = next.clone()
	if err := s.store.Save(ctx, Record{ID: rec.ID, Machine: &next, Request: rec.Request}); err != nil {
		return err
	}
	rec.Machine = &next
	return nil
}

// unrecorded handles an effect that happened but whose state could not be
// recorded: mvmd cannot establish the Machine's state, so it is Faulted.
func (s *service) unrecorded(rec *Record, err error) error {
	faulted := rec.Machine.clone()
	faulted.State = StateFaulted
	faulted.FailureReason = "the Machine's state could not be recorded: " + err.Error()
	rec.Machine = &faulted
	return fail(ErrFailed, "%s", faulted.FailureReason)
}

func (s *service) requireNotFaulted(m Machine, call string) error {
	if m.State == StateFaulted {
		return fail(ErrPreconditionFailed, "Machine %q is Faulted; %s does not apply, only stop and delete do", m.ID, call)
	}
	return nil
}

// holder returns the Machine holding a disk, if any does.
func (s *service) holder(disk DiskID) (MachineID, bool) {
	for _, rec := range s.records {
		if rec.Machine != nil && rec.Machine.holds(disk) {
			return rec.ID, true
		}
	}
	return "", false
}

func (s *service) Inspect(ctx context.Context, id MachineID) (Machine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, err := s.machine(id)
	if err != nil {
		return Machine{}, err
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
