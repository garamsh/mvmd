package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func (s *Server) CreateDisk(ctx context.Context, req *v1alpha2.CreateDiskRequest) (*v1alpha2.CreateDiskResponse, error) {
	if len(req.GetContents()) > 0 {
		return s.createDiskWithContents(ctx, req)
	}
	if req.GetSizeBytes() == 0 {
		return nil, rejected("size_bytes is zero")
	}
	disk := machine.DiskID(req.GetDiskId())
	fs, err := filesystemOf(req.GetFilesystem())
	if err != nil {
		return nil, err
	}
	if req.GetFromSnapshotId() != "" {
		return s.createDiskFromSnapshot(ctx, req, fs)
	}
	if err := s.machines.CreateDisk(ctx, disk, machine.RequestID(req.GetRequestId()), req.GetSizeBytes(), fs); err != nil {
		return nil, status(err)
	}
	if fs == "" {
		fs = machine.FilesystemExt4
	}
	return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: req.GetSizeBytes(), Filesystem: filesystemWire(fs)}}, nil
}

// createDiskWithContents makes a disk with no filesystem that holds the
// request's contents.
func (s *Server) createDiskWithContents(ctx context.Context, req *v1alpha2.CreateDiskRequest) (*v1alpha2.CreateDiskResponse, error) {
	contents := req.GetContents()
	switch {
	case len(contents) > machine.MaxDiskContents:
		return nil, rejected("contents is %d bytes, above %d", len(contents), machine.MaxDiskContents)
	case req.GetFilesystem() != v1alpha2.Filesystem_FILESYSTEM_NONE:
		return nil, rejected("contents is accepted only with filesystem FILESYSTEM_NONE")
	case false:
		return nil, rejected("contents is not accepted with from_snapshot_id")
	}
	disk := machine.DiskID(req.GetDiskId())
	if err := s.machines.CreateDiskWithContents(ctx, disk, machine.RequestID(req.GetRequestId()), req.GetSizeBytes(), contents); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: machine.ContentsDiskSize(req.GetSizeBytes(), len(contents)), Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE}}, nil
}

// createDiskFromSnapshot makes the disk from a snapshot. The disk takes the
// snapshot's filesystem, so the response reports what the disk holds.
func (s *Server) createDiskFromSnapshot(ctx context.Context, req *v1alpha2.CreateDiskRequest, fs machine.Filesystem) (*v1alpha2.CreateDiskResponse, error) {
	disk := machine.DiskID(req.GetDiskId())
	if err := s.machines.CreateDiskFromSnapshot(ctx, disk, machine.RequestID(req.GetRequestId()), req.GetSizeBytes(), fs, machine.DiskSnapshotID(req.GetFromSnapshotId())); err != nil {
		return nil, status(err)
	}
	all, err := s.machines.ListDisks(ctx)
	if err != nil {
		return nil, status(err)
	}
	for _, d := range all {
		if d.ID == disk {
			return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(d.ID), SizeBytes: d.SizeBytes, Filesystem: filesystemWire(d.Filesystem), HolderMachineId: string(d.Holder)}}, nil
		}
	}
	return &v1alpha2.CreateDiskResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: req.GetSizeBytes()}}, nil
}

func (s *Server) SnapshotDisk(ctx context.Context, req *v1alpha2.SnapshotDiskRequest) (*v1alpha2.SnapshotDiskResponse, error) {
	if err := s.machines.SnapshotDisk(ctx, machine.DiskID(req.GetDiskId()), machine.DiskSnapshotID(req.GetSnapshotId()), machine.RequestID(req.GetRequestId())); err != nil {
		return nil, status(err)
	}
	all, err := s.machines.ListDiskSnapshots(ctx)
	if err != nil {
		return nil, status(err)
	}
	for _, d := range all {
		if string(d.ID) == req.GetSnapshotId() {
			return &v1alpha2.SnapshotDiskResponse{DiskSnapshot: diskSnapshotToWire(d)}, nil
		}
	}
	return &v1alpha2.SnapshotDiskResponse{DiskSnapshot: &v1alpha2.DiskSnapshot{Id: req.GetSnapshotId(), DiskId: req.GetDiskId()}}, nil
}

func (s *Server) ListDiskSnapshots(ctx context.Context, _ *v1alpha2.ListDiskSnapshotsRequest) (*v1alpha2.ListDiskSnapshotsResponse, error) {
	all, err := s.machines.ListDiskSnapshots(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListDiskSnapshotsResponse{}
	for _, d := range all {
		resp.DiskSnapshots = append(resp.DiskSnapshots, diskSnapshotToWire(d))
	}
	return resp, nil
}

func (s *Server) DeleteDiskSnapshot(ctx context.Context, req *v1alpha2.DeleteDiskSnapshotRequest) (*v1alpha2.DeleteDiskSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, rejected("snapshot_id is empty")
	}
	if err := s.machines.DeleteDiskSnapshot(ctx, machine.DiskSnapshotID(req.GetSnapshotId())); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.DeleteDiskSnapshotResponse{}, nil
}

func diskSnapshotToWire(d machine.DiskSnapshotInfo) *v1alpha2.DiskSnapshot {
	return &v1alpha2.DiskSnapshot{
		Id:             string(d.ID),
		DiskId:         string(d.Disk),
		SizeBytes:      d.SizeBytes,
		Filesystem:     filesystemWire(d.Filesystem),
		AllocatedBytes: d.AllocatedBytes,
		CreatedAt:      timeToWire(d.CreatedAt),
	}
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

func (s *Server) ResizeDisk(ctx context.Context, req *v1alpha2.ResizeDiskRequest) (*v1alpha2.ResizeDiskResponse, error) {
	if req.GetDiskId() == "" {
		return nil, rejected("disk_id is empty")
	}
	if req.GetSizeBytes() == 0 {
		return nil, rejected("size_bytes is zero")
	}
	if err := s.machines.ResizeDisk(ctx, machine.DiskID(req.GetDiskId()), req.GetSizeBytes()); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.ResizeDiskResponse{}, nil
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
