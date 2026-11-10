package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
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
func (s *Server) view(ctx context.Context, id machine.MachineID) (*v1alpha2.Machine, error) {
	m, err := s.machines.Inspect(ctx, id)
	if err != nil {
		return nil, status(err)
	}
	return machineToWire(m), nil
}

func (s *Server) CreateMachine(ctx context.Context, req *v1alpha2.CreateMachineRequest) (*v1alpha2.CreateMachineResponse, error) {
	spec, err := specOf(req)
	if err != nil {
		return nil, rejected("%v", err)
	}
	id := machine.MachineID(req.GetMachineId())
	if err := s.machines.Create(ctx, id, machine.RequestID(req.GetRequestId()), spec); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.CreateMachineResponse{Machine: m}, err
}

func (s *Server) StartMachine(ctx context.Context, req *v1alpha2.StartMachineRequest) (*v1alpha2.StartMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Start(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.StartMachineResponse{Machine: m}, err
}

func (s *Server) StopMachine(ctx context.Context, req *v1alpha2.StopMachineRequest) (*v1alpha2.StopMachineResponse, error) {
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
	return &v1alpha2.StopMachineResponse{Machine: m}, err
}

func (s *Server) PauseMachine(ctx context.Context, req *v1alpha2.PauseMachineRequest) (*v1alpha2.PauseMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Pause(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.PauseMachineResponse{Machine: m}, err
}

func (s *Server) ResumeMachine(ctx context.Context, req *v1alpha2.ResumeMachineRequest) (*v1alpha2.ResumeMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Resume(ctx, id); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.ResumeMachineResponse{Machine: m}, err
}

func (s *Server) DeleteMachine(ctx context.Context, req *v1alpha2.DeleteMachineRequest) (*v1alpha2.DeleteMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.Delete(ctx, id); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.DeleteMachineResponse{}, nil
}

func (s *Server) SnapshotMachine(ctx context.Context, req *v1alpha2.SnapshotMachineRequest) (*v1alpha2.SnapshotMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	snapshot := machine.SnapshotID(req.GetSnapshotId())
	if err := s.machines.Snapshot(ctx, id, snapshot, machine.RequestID(req.GetRequestId())); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.SnapshotMachineResponse{SnapshotId: string(snapshot)}, nil
}

func (s *Server) RestoreMachine(ctx context.Context, req *v1alpha2.RestoreMachineRequest) (*v1alpha2.RestoreMachineResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, rejected("snapshot_id is empty")
	}
	var policy machine.NetworkPolicy
	var err error
	if policy.Egress, err = rulesOf(req.GetNetworkPolicy().GetEgress()); err != nil {
		return nil, rejected("%v", err)
	}
	if policy.Ingress, err = rulesOf(req.GetNetworkPolicy().GetIngress()); err != nil {
		return nil, rejected("%v", err)
	}
	id := machine.MachineID(req.GetMachineId())
	if err := s.machines.RestoreSnapshot(ctx, id, machine.RequestID(req.GetRequestId()), machine.SnapshotID(req.GetSnapshotId()), policy); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.RestoreMachineResponse{Machine: m}, err
}

func (s *Server) AttachDisk(ctx context.Context, req *v1alpha2.AttachDiskRequest) (*v1alpha2.AttachDiskResponse, error) {
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
	return &v1alpha2.AttachDiskResponse{Machine: m}, err
}

func (s *Server) DetachDisk(ctx context.Context, req *v1alpha2.DetachDiskRequest) (*v1alpha2.DetachDiskResponse, error) {
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
	return &v1alpha2.DetachDiskResponse{Machine: m}, err
}

func (s *Server) SetResources(ctx context.Context, req *v1alpha2.SetResourcesRequest) (*v1alpha2.SetResourcesResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if req.GetTarget() == nil {
		return nil, rejected("target is missing")
	}
	if err := s.machines.SetResources(ctx, id, resourcesOf(req.GetTarget()), resourcesOf(req.GetResourceCeiling())); err != nil {
		return nil, status(err)
	}
	m, err := s.view(ctx, id)
	if err != nil {
		return nil, err
	}
	return &v1alpha2.SetResourcesResponse{Resources: m.GetResources(), ResourceCeiling: m.GetResourceCeiling()}, nil
}

func (s *Server) InspectMachine(ctx context.Context, req *v1alpha2.InspectMachineRequest) (*v1alpha2.InspectMachineResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	m, err := s.view(ctx, id)
	return &v1alpha2.InspectMachineResponse{Machine: m}, err
}

func (s *Server) ListMachines(ctx context.Context, _ *v1alpha2.ListMachinesRequest) (*v1alpha2.ListMachinesResponse, error) {
	all, err := s.machines.List(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListMachinesResponse{}
	for _, m := range all {
		resp.Machines = append(resp.Machines, machineToWire(m))
	}
	return resp, nil
}
