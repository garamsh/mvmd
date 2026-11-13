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
	"net/netip"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// Version is the API version this server serves.
const Version = "mvmd.runtime.v1alpha2"

// Server implements v1alpha2.RuntimeServiceServer.
type Server struct {
	// Unsafe, not Unimplemented: an rpc added to the proto without a method
	// here fails the build instead of answering codes.Unimplemented.
	v1alpha2.UnsafeRuntimeServiceServer
	machines machine.Service
	deadline fencing.Service
	images   image.Service
	driver   machine.DriverInfo
	capacity nodecapacity.Reader
	events   events.Service
	kernels  machine.Kernels
	prefixes []netip.Prefix
	// stopping ends the event streams when Stop is called: a stream has no
	// end of its own, and a graceful stop of the gRPC server waits for it.
	stopping context.Context
	stop     context.CancelFunc
}

// New returns the server over the lifecycle, the deadline, the image pull use
// case and the event log, reporting the one Driver the node serves and the capacity the
// reader reads in runtime info.
func New(machines machine.Service, deadline fencing.Service, images image.Service, driver machine.DriverInfo, capacity nodecapacity.Reader, log events.Service, options ...Option) *Server {
	stopping, stop := context.WithCancel(context.Background())
	s := &Server{machines: machines, deadline: deadline, images: images, driver: driver, capacity: capacity, events: log, stopping: stopping, stop: stop}
	for _, o := range options {
		o(s)
	}
	return s
}

// Option changes what the server reports from its defaults.
type Option func(*Server)

// WithKernels sets the guest kernels runtime info lists (ADR 0045). Without
// it the node lists none.
func WithKernels(k machine.Kernels) Option {
	return func(s *Server) { s.kernels = k }
}

// WithMachinePrefixes sets the prefixes runtime info reports as the ones the
// node assigns Machine addresses from (ADR 0049). Without it the node lists
// none.
func WithMachinePrefixes(p ...netip.Prefix) Option {
	return func(s *Server) { s.prefixes = p }
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
	machine.CapabilitySnapshot:     {v1alpha2.Capability_CAPABILITY_SNAPSHOT},
	machine.CapabilityConsole:      {v1alpha2.Capability_CAPABILITY_CONSOLE},
	machine.CapabilityDiskHotplug:  {v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG},
	machine.CapabilityCPUResize:    {v1alpha2.Capability_CAPABILITY_CPU_RESIZE},
	machine.CapabilityMemoryResize: {v1alpha2.Capability_CAPABILITY_MEMORY_RESIZE},
	machine.CapabilityDiskResize:   {v1alpha2.Capability_CAPABILITY_DISK_RESIZE},
}

func (s *Server) GetRuntimeInfo(context.Context, *v1alpha2.GetRuntimeInfoRequest) (*v1alpha2.GetRuntimeInfoResponse, error) {
	have, err := s.capacity.Read()
	if err != nil {
		return nil, status(fmt.Errorf("read the node's capacity: %w", err))
	}
	resp := &v1alpha2.GetRuntimeInfoResponse{
		ApiVersions: []string{Version},
		Capacity: &v1alpha2.NodeCapacity{
			Cpus: have.CPUs, MemoryBytes: have.MemoryBytes,
			StateTotalBytes: have.StateTotalBytes, StateAvailableBytes: have.StateAvailableBytes,
			MachineLimit: have.MachineLimit, Machines: have.Machines,
		},
	}
	for _, p := range s.prefixes {
		resp.MachinePrefixes = append(resp.MachinePrefixes, p.Masked().String())
	}
	for _, name := range s.kernels.Names() {
		resp.Kernels = append(resp.Kernels, &v1alpha2.NodeKernel{Name: name, Digest: s.kernels.Offered[name].SHA256, IsDefault: name == s.kernels.Default})
	}
	d := s.driver
	info := &v1alpha2.DriverInfo{Driver: &v1alpha2.Driver{Name: d.Name, Version: d.Version}, CpuModel: d.CPUModel}
	if d.Limits != (machine.DriverLimits{}) {
		info.Limits = &v1alpha2.DriverLimits{MaxVcpus: d.Limits.MaxVCPUs}
	}
	for _, c := range d.Capabilities {
		info.Capabilities = append(info.Capabilities, wireCapabilities[c]...)
	}
	resp.Drivers = []*v1alpha2.DriverInfo{info}
	return resp, nil
}
