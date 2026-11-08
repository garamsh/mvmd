package api

import (
	"context"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// The calls the inside does not offer yet end UNSUPPORTED with the reason,
// never a panic and never a bare Unimplemented. Each names the issue that
// adds it.

func unsupported(call, issue string) error {
	return v1alpha1.NewError(v1alpha1.OutcomeKind_OUTCOME_KIND_UNSUPPORTED,
		call+" is not offered by this mvmd yet; it arrives with "+issue)
}

func (*Server) Console(grpc.BidiStreamingServer[v1alpha1.ConsoleRequest, v1alpha1.ConsoleResponse]) error {
	return unsupported("Console", "garamsh/mvmd#43")
}

func (*Server) PullImage(context.Context, *v1alpha1.PullImageRequest) (*v1alpha1.PullImageResponse, error) {
	return nil, unsupported("PullImage", "garamsh/mvmd#19")
}
