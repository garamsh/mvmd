package v1alpha2_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A bare ABORTED status is no kind: no call is answered "in progress", so the
// code does not read as one. A code a kind maps to still reads as it.
func TestKindOf_AbortedIsNoKind(t *testing.T) {
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_UNSPECIFIED, v1alpha2.KindOf(status.Error(codes.Aborted, "retry")))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(status.Error(codes.AlreadyExists, "control")))
}

// The gRPC status code of an error carries its kind for a caller that reads no
// details, as the proto states, and a code with a kind reads back as it.
func TestNewError_CarriesTheGRPCCodeTheProtoStates(t *testing.T) {
	tests := []struct {
		kind v1alpha2.OutcomeKind
		code codes.Code
	}{
		{v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, codes.InvalidArgument},
		{v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, codes.NotFound},
		{v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, codes.AlreadyExists},
		{v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, codes.FailedPrecondition},
		{v1alpha2.OutcomeKind_OUTCOME_KIND_UNSUPPORTED, codes.Unimplemented},
		{v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, codes.Internal},
	}
	for _, tt := range tests {
		t.Run(tt.kind.String(), func(t *testing.T) {
			err := v1alpha2.NewError(tt.kind, "reason")

			assert.Equal(t, tt.code, status.Code(err))
			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, tt.kind, v1alpha2.KindOf(status.Error(tt.code, "no details")), "a caller reading only the code")
		})
	}
}

// A read of exec output before the held window is answered with the earliest
// offset held, as data a client resumes from; any other failure carries none.
func TestEarliestOffset(t *testing.T) {
	gone := v1alpha2.NewOutputGone("the output before offset 4 is no longer held", 4)

	offset, ok := v1alpha2.EarliestOffset(gone)
	assert.True(t, ok)
	assert.Equal(t, uint64(4), offset)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(gone), "it is still a failed precondition")

	offset, ok = v1alpha2.EarliestOffset(v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, "other"))
	assert.False(t, ok, "control: a failed precondition without the detail has no offset")
	assert.Zero(t, offset)
	_, ok = v1alpha2.EarliestOffset(errors.New("plain"))
	assert.False(t, ok)
	_, ok = v1alpha2.EarliestOffset(nil)
	assert.False(t, ok)

	zero := v1alpha2.NewOutputGone("empty window", 0)
	offset, ok = v1alpha2.EarliestOffset(zero)
	assert.True(t, ok, "an earliest offset of zero is still an answer")
	assert.Zero(t, offset)
}
