package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

type probed struct {
	creator
	results []machine.ProbeResult
}

func (p *probed) Inspect(context.Context, machine.MachineID) (machine.Machine, error) {
	return machine.Machine{ID: "m", Spec: p.spec, ProbeResults: p.results}, nil
}

// CreateMachine carries the probes into the Spec with the durations and
// thresholds as sent, and Inspect reports each probe's result.
func TestCreateMachine_CarriesTheProbesAndInspectReportsTheirResults(t *testing.T) {
	changed := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	p := &probed{results: []machine.ProbeResult{
		{Name: "ready", Status: machine.ProbeFailing, ChangedAt: changed, ConsecutiveFailures: 3, LastFailure: "exit status 1", LastOutput: []byte("out")},
		{Name: "port"},
	}}
	server := api.New(p, nil, nil, machine.DriverInfo{}, nil, nil)

	_, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{
		MachineId: "m", RequestId: "r",
		Probes: []*v1alpha2.Probe{
			{Name: "ready", Kind: &v1alpha2.Probe_Exec{Exec: &v1alpha2.ExecProbe{Argv: []string{"/bin/check"}}},
				InitialDelay: durationpb.New(time.Second), Period: durationpb.New(5 * time.Second), Timeout: durationpb.New(time.Second), SuccessThreshold: 2, FailureThreshold: 4},
			{Name: "port", Kind: &v1alpha2.Probe_Tcp{Tcp: &v1alpha2.TcpProbe{Port: 8080}}},
			{Name: "web", Kind: &v1alpha2.Probe_HttpGet{HttpGet: &v1alpha2.HttpGetProbe{Port: 80, Path: "/h", Scheme: "http"}}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []machine.Probe{
		{Name: "ready", Kind: machine.ProbeExec, Argv: []string{"/bin/check"}, InitialDelay: time.Second, Period: 5 * time.Second, Timeout: time.Second, SuccessThreshold: 2, FailureThreshold: 4},
		{Name: "port", Kind: machine.ProbeTCP, Port: 8080},
		{Name: "web", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/h", Scheme: "http"},
	}, p.spec.Probes)

	got, err := server.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	require.Len(t, got.Machine.Probes, 2)
	assert.Equal(t, "ready", got.Machine.Probes[0].Name)
	assert.Equal(t, v1alpha2.ProbeStatus_PROBE_STATUS_FAILING, got.Machine.Probes[0].Status)
	assert.True(t, changed.Equal(got.Machine.Probes[0].ChangedAt.AsTime()))
	assert.Equal(t, uint32(3), got.Machine.Probes[0].ConsecutiveFailures)
	assert.Equal(t, "exit status 1", got.Machine.Probes[0].LastFailure)
	assert.Equal(t, []byte("out"), got.Machine.Probes[0].LastOutput)
	assert.Equal(t, v1alpha2.ProbeStatus_PROBE_STATUS_UNKNOWN, got.Machine.Probes[1].Status)

	// Control: none given, none carried.
	c := &probed{}
	_, err = api.New(c, nil, nil, machine.DriverInfo{}, nil, nil).CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{MachineId: "m", RequestId: "r"})
	require.NoError(t, err)
	assert.Empty(t, c.spec.Probes)
}

// watchStream collects what WatchEvents sends and ends the call after the
// first message.
type watchStream struct {
	grpc.ServerStream
	ctx    context.Context
	cancel context.CancelFunc
	sent   []*v1alpha2.WatchEventsResponse
}

func (w *watchStream) Context() context.Context { return w.ctx }
func (w *watchStream) Send(m *v1alpha2.WatchEventsResponse) error {
	w.sent = append(w.sent, m)
	w.cancel()
	return nil
}

// A probe change is streamed as a probe_changed event naming the Machine.
func TestWatchEvents_StreamsAProbeChange(t *testing.T) {
	log := events.NewService(10)
	log.ProbeChanged("m", machine.ProbeResult{Name: "ready", Status: machine.ProbePassing, ConsecutiveSuccesses: 1})
	server := api.New(nil, nil, nil, machine.DriverInfo{}, nil, log)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	stream := &watchStream{ctx: ctx, cancel: cancel}
	require.NoError(t, server.WatchEvents(&v1alpha2.WatchEventsRequest{}, stream))
	require.Len(t, stream.sent, 1)
	got := stream.sent[0]
	assert.Equal(t, "m", got.MachineId)
	assert.Equal(t, uint64(1), got.Sequence)
	pc := got.GetProbeChanged()
	require.NotNil(t, pc)
	assert.Equal(t, "ready", pc.Result.Name)
	assert.Equal(t, v1alpha2.ProbeStatus_PROBE_STATUS_PASSING, pc.Result.Status)
	assert.Equal(t, uint32(1), pc.Result.ConsecutiveSuccesses)
}
