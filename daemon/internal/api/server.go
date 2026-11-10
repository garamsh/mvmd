// Package api is the driving adapter for the runtime API: it serves
// RuntimeService from lib/runtimeapi/v1alpha2 over the inside, converting
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
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// Version is the API version this server serves.
const Version = "mvmd.runtime.v1alpha2"

// Server implements v1alpha2.RuntimeServiceServer.
type Server struct {
	v1alpha2.UnimplementedRuntimeServiceServer
	machines machine.Service
	deadline fencing.Service
	images   image.Service
	drivers  []machine.DriverInfo
	events   events.Service
	// stopping ends the event streams when Stop is called: a stream has no
	// end of its own, and a graceful stop of the gRPC server waits for it.
	stopping context.Context
	stop     context.CancelFunc
}

// New returns the server over the lifecycle, the deadline, the image pull use
// case and the event log, reporting the given Drivers in runtime info.
func New(machines machine.Service, deadline fencing.Service, images image.Service, drivers []machine.DriverInfo, log events.Service) *Server {
	stopping, stop := context.WithCancel(context.Background())
	return &Server{machines: machines, deadline: deadline, images: images, drivers: drivers, events: log, stopping: stopping, stop: stop}
}

// Stop ends every event stream, so the gRPC server can stop gracefully.
func (s *Server) Stop() { s.stop() }

// Register registers the server on a gRPC server.
func (s *Server) Register(g *grpc.Server) { v1alpha2.RegisterRuntimeServiceServer(g, s) }

// Interceptor refuses, with the REJECTED outcome, a request that fails its own
// Validate. The client refuses them too, but a server does not rely on it.
func Interceptor(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if v, ok := req.(interface{ Validate() error }); ok {
		if err := v.Validate(); err != nil {
			return nil, v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, err.Error())
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
			return v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, err.Error())
		}
	}
	return nil
}

func rejected(format string, args ...any) error {
	return v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, fmt.Sprintf(format, args...))
}

// status maps an error of the inside to the call's outcome.
func status(err error) error {
	if err == nil {
		return nil
	}
	kind := v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED
	reason := err.Error()
	var gone *machine.OutputGoneError
	if errors.As(err, &gone) {
		return v1alpha2.NewOutputGone(gone.Error(), gone.Earliest)
	}
	var outcome *machine.Error
	if errors.As(err, &outcome) {
		reason = outcome.Reason
		switch {
		case errors.Is(outcome, machine.ErrInvalid):
			kind = v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED
		case errors.Is(outcome, machine.ErrNotFound):
			kind = v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND
		case errors.Is(outcome, machine.ErrConflict):
			kind = v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT
		case errors.Is(outcome, machine.ErrPreconditionFailed):
			kind = v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED
		case errors.Is(outcome, machine.ErrUnsupported):
			kind = v1alpha2.OutcomeKind_OUTCOME_KIND_UNSUPPORTED
		}
	}
	return v1alpha2.NewError(kind, reason)
}

// wireCapabilities maps each Driver capability to the API values that report
// it. A capability with no entry would be dropped from the runtime info, so a
// test fails for one.
var wireCapabilities = map[machine.Capability][]v1alpha2.Capability{
	machine.CapabilitySnapshot:    {v1alpha2.Capability_CAPABILITY_SNAPSHOT},
	machine.CapabilityConsole:     {v1alpha2.Capability_CAPABILITY_CONSOLE},
	machine.CapabilityDiskHotplug: {v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG},
	machine.CapabilityResize:      {v1alpha2.Capability_CAPABILITY_CPU_RESIZE, v1alpha2.Capability_CAPABILITY_MEMORY_RESIZE},
}

func (s *Server) GetRuntimeInfo(context.Context, *v1alpha2.GetRuntimeInfoRequest) (*v1alpha2.GetRuntimeInfoResponse, error) {
	resp := &v1alpha2.GetRuntimeInfoResponse{ApiVersions: []string{Version}}
	for _, d := range s.drivers {
		info := &v1alpha2.DriverInfo{Driver: &v1alpha2.Driver{Name: d.Name, Version: d.Version}, CpuModel: d.CPUModel}
		for _, c := range d.Capabilities {
			info.Capabilities = append(info.Capabilities, wireCapabilities[c]...)
		}
		resp.Drivers = append(resp.Drivers, info)
	}
	return resp, nil
}
