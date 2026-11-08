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

func (*Server) SnapshotMachine(context.Context, *v1alpha1.SnapshotMachineRequest) (*v1alpha1.SnapshotMachineResponse, error) {
	return nil, unsupported("SnapshotMachine", "garamsh/mvmd#19")
}

func (*Server) RestoreMachine(context.Context, *v1alpha1.RestoreMachineRequest) (*v1alpha1.RestoreMachineResponse, error) {
	return nil, unsupported("RestoreMachine", "garamsh/mvmd#19")
}

func (*Server) Exec(context.Context, *v1alpha1.ExecRequest) (*v1alpha1.ExecResponse, error) {
	return nil, unsupported("Exec", "garamsh/mvmd#19")
}

func (*Server) ReadExecOutput(*v1alpha1.ReadExecOutputRequest, grpc.ServerStreamingServer[v1alpha1.ReadExecOutputResponse]) error {
	return unsupported("ReadExecOutput", "garamsh/mvmd#19")
}

func (*Server) ReadFile(*v1alpha1.ReadFileRequest, grpc.ServerStreamingServer[v1alpha1.ReadFileResponse]) error {
	return unsupported("ReadFile", "garamsh/mvmd#19")
}

func (*Server) WriteFile(grpc.ClientStreamingServer[v1alpha1.WriteFileRequest, v1alpha1.WriteFileResponse]) error {
	return unsupported("WriteFile", "garamsh/mvmd#19")
}

func (*Server) PatchFile(context.Context, *v1alpha1.PatchFileRequest) (*v1alpha1.PatchFileResponse, error) {
	return nil, unsupported("PatchFile", "garamsh/mvmd#19")
}

func (*Server) SearchFiles(context.Context, *v1alpha1.SearchFilesRequest) (*v1alpha1.SearchFilesResponse, error) {
	return nil, unsupported("SearchFiles", "garamsh/mvmd#19")
}

func (*Server) Console(grpc.BidiStreamingServer[v1alpha1.ConsoleRequest, v1alpha1.ConsoleResponse]) error {
	return unsupported("Console", "garamsh/mvmd#19")
}

func (*Server) PullImage(context.Context, *v1alpha1.PullImageRequest) (*v1alpha1.PullImageResponse, error) {
	return nil, unsupported("PullImage", "garamsh/mvmd#19")
}
