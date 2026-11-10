package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func (s *Server) ListSnapshots(ctx context.Context, _ *v1alpha2.ListSnapshotsRequest) (*v1alpha2.ListSnapshotsResponse, error) {
	all, err := s.machines.ListSnapshots(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListSnapshotsResponse{}
	for _, snapshot := range all {
		resp.Snapshots = append(resp.Snapshots, snapshotToWire(snapshot))
	}
	return resp, nil
}

func (s *Server) DeleteSnapshot(ctx context.Context, req *v1alpha2.DeleteSnapshotRequest) (*v1alpha2.DeleteSnapshotResponse, error) {
	if req.GetSnapshotId() == "" {
		return nil, rejected("snapshot_id is empty")
	}
	if err := s.machines.DeleteSnapshot(ctx, machine.SnapshotID(req.GetSnapshotId())); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.DeleteSnapshotResponse{}, nil
}

func snapshotToWire(snapshot machine.Snapshot) *v1alpha2.Snapshot {
	return &v1alpha2.Snapshot{
		Id:            string(snapshot.ID),
		MachineId:     string(snapshot.Machine),
		ImageDigest:   string(snapshot.Spec.Image),
		Driver:        &v1alpha2.Driver{Name: snapshot.Compat.Driver, Version: snapshot.Compat.Version},
		CpuModel:      snapshot.Compat.CPUModel,
		RootDiskBytes: snapshot.Spec.RootDiskBytes,
	}
}
