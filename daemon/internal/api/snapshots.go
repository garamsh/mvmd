package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

func (s *Server) ListSnapshots(ctx context.Context, _ *v1alpha1.ListSnapshotsRequest) (*v1alpha1.ListSnapshotsResponse, error) {
	all, err := s.machines.ListSnapshots(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha1.ListSnapshotsResponse{}
	for _, snapshot := range all {
		resp.Snapshots = append(resp.Snapshots, snapshotToWire(snapshot))
	}
	return resp, nil
}

func (s *Server) DeleteSnapshot(ctx context.Context, req *v1alpha1.DeleteSnapshotRequest) (*v1alpha1.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, rejected("snapshot_id is empty")
	}
	if err := s.machines.DeleteSnapshot(ctx, machine.SnapshotID(req.GetSnapshotId())); err != nil {
		return nil, status(err)
	}
	return &v1alpha1.DeleteSnapshotResponse{}, nil
}

func snapshotToWire(snapshot machine.Snapshot) *v1alpha1.Snapshot {
	return &v1alpha1.Snapshot{
		Id:            string(snapshot.ID),
		MachineId:     string(snapshot.Machine),
		ImageDigest:   string(snapshot.Spec.Image),
		Driver:        &v1alpha1.Driver{Name: snapshot.Compat.Driver, Version: snapshot.Compat.Version},
		CpuModel:      snapshot.Compat.CPUModel,
		RootDiskBytes: snapshot.Spec.RootDiskBytes,
	}
}
