package api

import (
	"context"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func (s *Server) RenewDeadline(ctx context.Context, req *v1alpha2.RenewDeadlineRequest) (*v1alpha2.RenewDeadlineResponse, error) {
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
	return &v1alpha2.RenewDeadlineResponse{Status: deadlineToWire(st)}, nil
}

func (s *Server) InspectDeadline(ctx context.Context, _ *v1alpha2.InspectDeadlineRequest) (*v1alpha2.InspectDeadlineResponse, error) {
	st, err := s.deadline.Inspect(ctx)
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha2.InspectDeadlineResponse{Status: deadlineToWire(st)}, nil
}
