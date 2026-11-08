package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// target names the Machine of a request, which every call except the creating
// one and the reads of all Machines needs.
func target(id string) (machine.MachineID, error) {
	if id == "" {
		return "", rejected("machine_id is empty")
	}
	return machine.MachineID(id), nil
}

// view reads the Machine back as the response to a call that changed it.
func (s *Server) view(ctx context.Context, id machine.MachineID) (*v1alpha1.Machine, error) {
	m, err := s.machines.Inspect(ctx, id)
	if err != nil {
		return nil, status(err)
	}
	return machineToWire(m), nil
}

func (s *Server) CreateMachine(ctx context.Context, req *v1alpha1.CreateMachineRequest) (*v1alpha1.CreateMachineResponse, error) {
	spec, err := specOf(req)
	if err != nil {
		return nil, rejected("%v", err)
	}
	id := machine.MachineID(req.GetMachineId())
	if err := s.machines.Create(ctx, id, machine.RequestID(req.GetRequestId()), spec); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.CreateMachineResponse{Machine: m}, err
}

func (s *Server) StartMachine(ctx context.Context, req *v1alpha1.StartMachineRequest) (*v1alpha1.StartMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Start(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.StartMachineResponse{Machine: m}, err
}

func (s *Server) StopMachine(ctx context.Context, req *v1alpha1.StopMachineRequest) (*v1alpha1.StopMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if g := req.GetGracePeriod(); g != nil {
		if err := g.CheckValid(); err != nil {
			return nil, rejected("grace_period: %v", err)
		}
		if g.AsDuration() < 0 {
			return nil, rejected("grace_period is negative")
		}
	}
	if err := s.machines.Stop(ctx, id, req.GetGracePeriod().AsDuration()); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.StopMachineResponse{Machine: m}, err
}

func (s *Server) PauseMachine(ctx context.Context, req *v1alpha1.PauseMachineRequest) (*v1alpha1.PauseMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Pause(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.PauseMachineResponse{Machine: m}, err
}

func (s *Server) ResumeMachine(ctx context.Context, req *v1alpha1.ResumeMachineRequest) (*v1alpha1.ResumeMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Resume(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.ResumeMachineResponse{Machine: m}, err
}

func (s *Server) DeleteMachine(ctx context.Context, req *v1alpha1.DeleteMachineRequest) (*v1alpha1.DeleteMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Delete(ctx, id); err != nil {
		return nil, status(err)
	}
	return &v1alpha1.DeleteMachineResponse{}, nil
}

func (s *Server) AttachDisk(ctx context.Context, req *v1alpha1.AttachDiskRequest) (*v1alpha1.AttachDiskResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if req.GetDisk().GetDiskId() == "" {
		return nil, rejected("disk.disk_id is empty")
	}
	attachment := machine.DiskAttachment{Disk: machine.DiskID(req.GetDisk().GetDiskId()), ReadOnly: req.GetDisk().GetReadOnly()}
	if err := s.machines.AttachDisk(ctx, id, attachment); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.AttachDiskResponse{Machine: m}, err
}

func (s *Server) DetachDisk(ctx context.Context, req *v1alpha1.DetachDiskRequest) (*v1alpha1.DetachDiskResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if req.GetDiskId() == "" {
		return nil, rejected("disk_id is empty")
	}
	if err := s.machines.DetachDisk(ctx, id, machine.DiskID(req.GetDiskId())); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.DetachDiskResponse{Machine: m}, err
}

func (s *Server) SetResources(ctx context.Context, req *v1alpha1.SetResourcesRequest) (*v1alpha1.SetResourcesResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if req.GetTarget() == nil {
		return nil, rejected("target is missing")
	}
	if err := s.machines.SetResources(ctx, id, resourcesOf(req.GetTarget())); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	if err != nil {
		return nil, err
	}
	return &v1alpha1.SetResourcesResponse{Resources: m.GetResources()}, nil
}

func (s *Server) InspectMachine(ctx context.Context, req *v1alpha1.InspectMachineRequest) (*v1alpha1.InspectMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	m, err := s.view(ctx, id)
	return &v1alpha1.InspectMachineResponse{Machine: m}, err
}

func (s *Server) ListMachines(ctx context.Context, _ *v1alpha1.ListMachinesRequest) (*v1alpha1.ListMachinesResponse, error) {
	all, err := s.machines.List(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha1.ListMachinesResponse{}
	for _, m := range all {
		resp.Machines = append(resp.Machines, machineToWire(m))
	}
	return resp, nil
}
