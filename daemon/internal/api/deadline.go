package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func (s *Server) RenewDeadline(ctx context.Context, req *v1alpha2.RenewDeadlineRequest) (*v1alpha2.RenewDeadlineResponse, error) {
	if req.GetLength() == nil {
		return nil, rejected("length is missing")
	}
	if err := req.GetLength().CheckValid(); err != nil {
		return nil, rejected("length: %v", err)
	}
	var st fencing.Status
	var err error
	if req.GetMachineId() != "" {
		var id machine.MachineID
		if id, err = s.fenced(ctx, req.GetMachineId()); err != nil {
			return nil, err
		}
		st, err = s.deadline.RenewFence(ctx, id, req.GetLength().AsDuration(), req.GetRenewalNumber())
	} else {
		st, err = s.deadline.Renew(ctx, req.GetLength().AsDuration(), req.GetRenewalNumber())
	}
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha2.RenewDeadlineResponse{Status: deadlineToWire(st)}, nil
}

func (s *Server) InspectDeadline(ctx context.Context, req *v1alpha2.InspectDeadlineRequest) (*v1alpha2.InspectDeadlineResponse, error) {
	var st fencing.Status
	var err error
	if req.GetMachineId() != "" {
		var id machine.MachineID
		if id, err = s.fenced(ctx, req.GetMachineId()); err != nil {
			return nil, err
		}
		st, err = s.deadline.InspectFence(ctx, id)
	} else {
		st, err = s.deadline.Inspect(ctx)
	}
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha2.InspectDeadlineResponse{Status: deadlineToWire(st)}, nil
}

// fenced returns the Machine a call on its own fence names, refusing one that
// does not exist, or that was created without a fence (ADR 0064).
func (s *Server) fenced(ctx context.Context, name string) (machine.MachineID, error) {
	id, err := target(name)
	if err != nil {
		return "", err
	}
	m, err := s.machines.Inspect(ctx, id)
	if err != nil {
		return "", status(err)
	}
	if !m.Fenced {
		return "", status(machine.NewError(machine.ErrPreconditionFailed, "Machine %q has no fence of its own", id))
	}
	return id, nil
}
