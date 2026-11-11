package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// PullImage makes the image present in the node's cache. The use case's
// errors reach the caller through status, like every other call's.
func (s *Server) PullImage(ctx context.Context, req *v1alpha2.PullImageRequest) (*v1alpha2.PullImageResponse, error) {
	ref := image.Reference{Repository: req.GetImage().GetRepository(), Digest: machine.ImageDigest(req.GetImage().GetDigest())}
	if err := s.images.Pull(ctx, ref); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.PullImageResponse{}, nil
}

// ListImages and RemoveImage go to the lifecycle, not the image use case: a
// removal needs the Machine records, the snapshots and the locks Create and
// restore hold.
func (s *Server) ListImages(ctx context.Context, _ *v1alpha2.ListImagesRequest) (*v1alpha2.ListImagesResponse, error) {
	all, err := s.machines.ListImages(ctx)
	if err != nil {
		return nil, status(err)
	}
	resp := &v1alpha2.ListImagesResponse{}
	for _, info := range all {
		resp.Images = append(resp.Images, &v1alpha2.Image{Digest: string(info.Digest), SizeBytes: info.SizeBytes, CreatedAt: timeToWire(info.CreatedAt)})
	}
	return resp, nil
}

func (s *Server) RemoveImage(ctx context.Context, req *v1alpha2.RemoveImageRequest) (*v1alpha2.RemoveImageResponse, error) {
	if req.GetDigest() == "" {
		return nil, rejected("digest is empty")
	}
	if err := s.machines.RemoveImage(ctx, machine.ImageDigest(req.GetDigest())); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.RemoveImageResponse{}, nil
}
