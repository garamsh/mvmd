package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// Each domain kind a use case fails with is answered in the outcome kind
// runtime-api.md names, with the gRPC code the proto states for it.
func TestStatus_MapsEachDomainKindToItsOutcomeKind(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
		code codes.Code
	}{
		{"invalid", machine.NewError(machine.ErrInvalid, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, codes.InvalidArgument},
		{"not found", machine.NewError(machine.ErrNotFound, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, codes.NotFound},
		{"conflict", machine.NewError(machine.ErrConflict, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, codes.AlreadyExists},
		{"precondition failed", machine.NewError(machine.ErrPreconditionFailed, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, codes.FailedPrecondition},
		{"unsupported", machine.NewError(machine.ErrUnsupported, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_UNSUPPORTED, codes.Unimplemented},
		{"failed", machine.NewError(machine.ErrFailed, "r"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, codes.Internal},
		{"untyped", errors.New("boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &diskKeeper{err: tt.err}

			_, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).DeleteDisk(context.Background(), &v1alpha2.DeleteDiskRequest{DiskId: "data"})

			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, tt.code, grpcstatus.Code(err))
		})
	}
}
