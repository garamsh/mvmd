package api

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// PullImage makes the image present in the node's cache. The use case's
// errors reach the caller through status, like every other call's.
func (s *Server) PullImage(ctx context.Context, req *v1alpha1.PullImageRequest) (*v1alpha1.PullImageResponse, error) {
	ref := image.Reference{Repository: req.GetImage().GetRepository(), Digest: machine.ImageDigest(req.GetImage().GetDigest())}
	if err := s.images.Pull(ctx, ref); err != nil {
		return nil, status(err)
	}
	return &v1alpha1.PullImageResponse{}, nil
}
