package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func (s *Server) CreateDisk(ctx context.Context, req *v1alpha2.CreateDiskRequest) (*v1alpha2.CreateDiskResponse, error) {
	if req.GetSizeBytes() == 0 {
		return nil, rejected("size_bytes is zero")
	}
	disk := machine.DiskID(req.GetDiskId())
	if err := s.machines.CreateDisk(ctx, disk, machine.RequestID(req.GetRequestId()), req.GetSizeBytes()); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: req.GetSizeBytes()}}, nil
}

func (s *Server) ListDisks(ctx context.Context, _ *v1alpha2.ListDisksRequest) (*v1alpha2.ListDisksResponse, error) {
	all, err := s.machines.ListDisks(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListDisksResponse{}
	for _, d := range all {
		resp.Disks = append(resp.Disks, &v1alpha2.Disk{Id: string(d.ID), SizeBytes: d.SizeBytes, HolderMachineId: string(d.Holder)})
	}
	return resp, nil
}

func (s *Server) DeleteDisk(ctx context.Context, req *v1alpha2.DeleteDiskRequest) (*v1alpha2.DeleteDiskResponse, error) {
	if req.GetDiskId() == "" {
		return nil, rejected("disk_id is empty")
	}
	if err := s.machines.DeleteDisk(ctx, machine.DiskID(req.GetDiskId())); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.DeleteDiskResponse{}, nil
}
