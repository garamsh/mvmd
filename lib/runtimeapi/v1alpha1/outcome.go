package v1alpha1

import (
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var codeOfKind = map[OutcomeKind]codes.Code{
	OutcomeKind_OUTCOME_KIND_REJECTED:            codes.InvalidArgument,
	OutcomeKind_OUTCOME_KIND_NOT_FOUND:           codes.NotFound,
	OutcomeKind_OUTCOME_KIND_CONFLICT:            codes.AlreadyExists,
	OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED: codes.FailedPrecondition,
	OutcomeKind_OUTCOME_KIND_UNSUPPORTED:         codes.Unimplemented,
	OutcomeKind_OUTCOME_KIND_FAILED:              codes.Internal,
	OutcomeKind_OUTCOME_KIND_IN_PROGRESS:         codes.Aborted,
}

// NewError returns the error that ends a call in the given non-success
// outcome kind, carrying the reason as a Failure detail.
func NewError(kind OutcomeKind, reason string) error {
	code, ok := codeOfKind[kind]
	if !ok {
		code = codes.Unknown
	}
	st := status.New(code, reason)
	withDetail, err := st.WithDetails(&Failure{Kind: kind, Reason: reason})
	if err != nil {
		return st.Err()
	}
	return withDetail.Err()
}

// KindOf returns the outcome kind an error ended a call in: SUCCESS for nil,
// the Failure detail's kind when the status carries one, otherwise the kind
// the status code maps to, and UNSPECIFIED for a code with no kind.
func KindOf(err error) OutcomeKind {
	if err == nil {
		return OutcomeKind_OUTCOME_KIND_SUCCESS
	}
	var withStatus interface{ GRPCStatus() *status.Status }
	if !errors.As(err, &withStatus) {
		return OutcomeKind_OUTCOME_KIND_UNSPECIFIED
	}
	st := withStatus.GRPCStatus()
	for _, detail := range st.Details() {
		if failure, ok := detail.(*Failure); ok {
			return failure.GetKind()
		}
	}
	for kind, code := range codeOfKind {
		if code == st.Code() {
			return kind
		}
	}
	return OutcomeKind_OUTCOME_KIND_UNSPECIFIED
}
