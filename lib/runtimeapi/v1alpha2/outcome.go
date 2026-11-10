package v1alpha2

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

// NewOutputGone returns the failed-precondition error that answers a read of
// exec output before the earliest offset still held, carrying that offset as
// an OutputGone detail beside the Failure.
func NewOutputGone(reason string, earliest uint64) error {
	st := status.New(codes.FailedPrecondition, reason)
	withDetails, err := st.WithDetails(
		&Failure{Kind: OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, Reason: reason},
		&OutputGone{EarliestOffset: earliest},
	)
	if err != nil {
		return st.Err()
	}
	return withDetails.Err()
}

// EarliestOffset returns the earliest offset an OutputGone detail of the error
// names, and whether the error carries one.
func EarliestOffset(err error) (uint64, bool) {
	var withStatus interface{ GRPCStatus() *status.Status }
	if err == nil || !errors.As(err, &withStatus) {
		return 0, false
	}
	for _, detail := range withStatus.GRPCStatus().Details() {
		if gone, ok := detail.(*OutputGone); ok {
			return gone.GetEarliestOffset(), true
		}
	}
	return 0, false
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
