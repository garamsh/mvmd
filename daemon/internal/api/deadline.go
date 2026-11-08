package api

import (
	"context"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

func (s *Server) RenewDeadline(ctx context.Context, req *v1alpha1.RenewDeadlineRequest) (*v1alpha1.RenewDeadlineResponse, error) {
	if req.GetLength() == nil {
		return nil, rejected("length is missing")
	}
	if err := req.GetLength().CheckValid(); err != nil {
		return nil, rejected("length: %v", err)
	}
	st, err := s.deadline.Renew(ctx, req.GetLength().AsDuration(), req.GetRenewalNumber())
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha1.RenewDeadlineResponse{Status: deadlineToWire(st)}, nil
}

func (s *Server) InspectDeadline(ctx context.Context, _ *v1alpha1.InspectDeadlineRequest) (*v1alpha1.InspectDeadlineResponse, error) {
	st, err := s.deadline.Inspect(ctx)
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha1.InspectDeadlineResponse{Status: deadlineToWire(st)}, nil
}
