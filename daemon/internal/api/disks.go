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
	fs, err := filesystemOf(req.GetFilesystem())
	if err != nil {
		return nil, err
	}
	if err := s.machines.CreateDisk(ctx, disk, machine.RequestID(req.GetRequestId()), req.GetSizeBytes(), fs); err != nil {
		return nil, status(err)
	}
	if fs == "" {
		fs = machine.FilesystemExt4
	}
	return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: req.GetSizeBytes(), Filesystem: filesystemWire(fs)}}, nil
}

func (s *Server) ListDisks(ctx context.Context, _ *v1alpha2.ListDisksRequest) (*v1alpha2.ListDisksResponse, error) {
	all, err := s.machines.ListDisks(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListDisksResponse{}
	for _, d := range all {
		resp.Disks = append(resp.Disks, &v1alpha2.Disk{Id: string(d.ID), SizeBytes: d.SizeBytes, Filesystem: filesystemWire(d.Filesystem), HolderMachineId: string(d.Holder)})
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

// filesystemOf maps the wire filesystem to the inside's; unspecified is empty,
// which CreateDisk reads as ext4.
func filesystemOf(f v1alpha2.Filesystem) (machine.Filesystem, error) {
	switch f {
	case v1alpha2.Filesystem_FILESYSTEM_UNSPECIFIED:
		return "", nil
	case v1alpha2.Filesystem_FILESYSTEM_NONE:
		return machine.FilesystemNone, nil
	case v1alpha2.Filesystem_FILESYSTEM_EXT4:
		return machine.FilesystemExt4, nil
	}
	return "", rejected("filesystem %d is not known", int32(f))
}

func filesystemWire(f machine.Filesystem) v1alpha2.Filesystem {
	switch f {
	case machine.FilesystemNone:
		return v1alpha2.Filesystem_FILESYSTEM_NONE
	case machine.FilesystemExt4:
		return v1alpha2.Filesystem_FILESYSTEM_EXT4
	}
	return v1alpha2.Filesystem_FILESYSTEM_UNSPECIFIED
}
