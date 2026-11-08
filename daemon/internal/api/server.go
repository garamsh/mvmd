// Package api is the driving adapter for the runtime API: it serves
// RuntimeService from lib/runtimeapi/v1alpha1 over the inside, converting
// between the wire types and the inside's, and mapping every outcome to its
// status code. It is the one place that imports lib/runtimeapi (structure.md
// §Repository layout).
package api

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// Version is the API version this server serves.
const Version = "mvmd.runtime.v1alpha1"

// Server implements v1alpha1.RuntimeServiceServer.
type Server struct {
	v1alpha1.UnimplementedRuntimeServiceServer
	machines machine.Service
	deadline fencing.Service
	drivers  []machine.DriverInfo
	events   events.Store
}

// New returns the server over the lifecycle and the deadline, reporting the
// given Drivers in runtime info.
func New(machines machine.Service, deadline fencing.Service, drivers []machine.DriverInfo, evts events.Store) *Server {
	return &Server{machines: machines, deadline: deadline, drivers: drivers, events: evts}
}

// Register registers the server on a gRPC server.
func (s *Server) Register(g *grpc.Server) { v1alpha1.RegisterRuntimeServiceServer(g, s) }

// Interceptor refuses, with the REJECTED outcome, a request that fails its own
// Validate. The client refuses them too, but a server does not rely on it.
func Interceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if v, ok := req.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, v1alpha1.NewError(v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, err.Error())
		}
	}
	return handler(ctx, req)
}

// StreamInterceptor wraps the server stream so that every message received
// through RecvMsg is validated; an invalid message is refused as REJECTED.
func StreamInterceptor(srv any, ss grpc.ServerStream, _ *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	wrapped := &validatingStream{ServerStream: ss}
	return handler(srv, wrapped)
}

type validatingStream struct {
	grpc.ServerStream
}

func (v *validatingStream) RecvMsg(m any) error {
	if err := v.ServerStream.RecvMsg(m); err != nil {
		return err
	}
	if val, ok := m.(interface{ Validate() error }); ok {
		if err := val.Validate(); err != nil {
			return v1alpha1.NewError(v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, err.Error())
		}
	}
	return nil
}

func rejected(format string, args ...any) error {
	return v1alpha1.NewError(v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED, fmt.Sprintf(format, args...))
}

// status maps an error of the inside to the call's outcome.
func status(err error) error {
	if err == nil {
		return nil
	}
	kind := v1alpha1.OutcomeKind_OUTCOME_KIND_FAILED
	reason := err.Error()
	var outcome *machine.Error
	if errors.As(err, &outcome) {
		reason = outcome.Reason
		switch {
		case errors.Is(outcome, machine.ErrInvalid):
			kind = v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED
		case errors.Is(outcome, machine.ErrNotFound):
			kind = v1alpha1.OutcomeKind_OUTCOME_KIND_NOT_FOUND
		case errors.Is(outcome, machine.ErrConflict):
			kind = v1alpha1.OutcomeKind_OUTCOME_KIND_CONFLICT
		case errors.Is(outcome, machine.ErrPreconditionFailed):
			kind = v1alpha1.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED
		case errors.Is(outcome, machine.ErrUnsupported):
			kind = v1alpha1.OutcomeKind_OUTCOME_KIND_UNSUPPORTED
		}
	}
	return v1alpha1.NewError(kind, reason)
}

func (s *Server) GetRuntimeInfo(context.Context, *v1alpha1.GetRuntimeInfoRequest) (*v1alpha1.GetRuntimeInfoResponse, error) {
	resp := &v1alpha1.GetRuntimeInfoResponse{ApiVersions: []string{Version}}
	for _, d := range s.drivers {
		info := &v1alpha1.DriverInfo{Driver: &v1alpha1.Driver{Name: d.Name, Version: d.Version}}
		for _, c := range d.Capabilities {
			switch c {
			case machine.CapabilityDiskHotplug:
				info.Capabilities = append(info.Capabilities, v1alpha1.Capability_CAPABILITY_DISK_HOTPLUG)
			case machine.CapabilityResize:
				info.Capabilities = append(info.Capabilities,
					v1alpha1.Capability_CAPABILITY_CPU_RESIZE, v1alpha1.Capability_CAPABILITY_MEMORY_RESIZE)
			}
		}
		resp.Drivers = append(resp.Drivers, info)
	}
	return resp, nil
}
