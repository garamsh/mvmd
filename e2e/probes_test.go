//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func execProbe(name string, argv ...string) *v1alpha2.Probe {
	return &v1alpha2.Probe{
		Name: name, Kind: &v1alpha2.Probe_Exec{Exec: &v1alpha2.ExecProbe{Argv: argv}},
		Period: durationpb.New(200 * time.Millisecond), FailureThreshold: 1,
	}
}

func tcpProbe(name string, port uint32) *v1alpha2.Probe {
	return &v1alpha2.Probe{
		Name: name, Kind: &v1alpha2.Probe_Tcp{Tcp: &v1alpha2.TcpProbe{Port: port}},
		Period: durationpb.New(200 * time.Millisecond), Timeout: durationpb.New(time.Second), FailureThreshold: 1,
	}
}

func httpProbe(name string, port uint32, path string) *v1alpha2.Probe {
	return &v1alpha2.Probe{
		Name: name, Kind: &v1alpha2.Probe_HttpGet{HttpGet: &v1alpha2.HttpGetProbe{Port: port, Path: path}},
		Period: durationpb.New(200 * time.Millisecond), Timeout: durationpb.New(time.Second), FailureThreshold: 1,
	}
}

// probeStatuses is what Inspect reports of the Machine's probes, by name.
func (n *node) probeStatuses(t *testing.T, ctx context.Context, machine string) map[string]v1alpha2.ProbeStatus {
	t.Helper()
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: machine})
	require.NoError(t, err)
	out := map[string]v1alpha2.ProbeStatus{}
	for _, p := range inspected.Machine.Probes {
		out[p.Name] = p.Status
	}
	return out
}

// A Machine created with probes reports each one's result while it runs, streams a probe's change
// as an event, and reports every probe Unknown once it is stopped.
func TestE2E_ProbesAreRunAndTheirResultsReportedAndStreamed(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "probed", []string{"serve", "8080"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	stream, err := n.client.WatchEvents(watchCtx, &v1alpha2.WatchEventsRequest{})
	require.NoError(t, err)
	changes := make(chan *v1alpha2.ProbeResult, 64)
	go func() {
		for {
			event, err := stream.Recv()
			if err != nil {
				return
			}
			if c := event.GetProbeChanged(); c != nil && event.MachineId == "probed" {
				select {
				case changes <- c.Result:
				case <-watchCtx.Done():
					return
				}
			}
		}
	}()

	req := createRequest("probed", pulled.Digest)
	req.Probes = []*v1alpha2.Probe{
		tcpProbe("listening", 8080),
		tcpProbe("closed", 9),
		httpProbe("http-ok", 8080, "/ok"),
		httpProbe("http-bad", 8080, "/broken"),
		httpProbe("http-missing", 8080, "/missing"),
		execProbe("exec-ok", "/bin/entry", "exit", "0"),
		execProbe("exec-bad", "/bin/entry", "exit", "1"),
	}
	n.runMachine(t, ctx, req)

	want := map[string]v1alpha2.ProbeStatus{
		"listening":    v1alpha2.ProbeStatus_PROBE_STATUS_PASSING,
		"closed":       v1alpha2.ProbeStatus_PROBE_STATUS_FAILING,
		"http-ok":      v1alpha2.ProbeStatus_PROBE_STATUS_PASSING,
		"http-bad":     v1alpha2.ProbeStatus_PROBE_STATUS_FAILING,
		"http-missing": v1alpha2.ProbeStatus_PROBE_STATUS_FAILING,
		"exec-ok":      v1alpha2.ProbeStatus_PROBE_STATUS_PASSING,
		"exec-bad":     v1alpha2.ProbeStatus_PROBE_STATUS_FAILING,
	}
	require.Eventually(t, func() bool {
		got := n.probeStatuses(t, ctx, "probed")
		for name, status := range want {
			if got[name] != status {
				return false
			}
		}
		return true
	}, 2*time.Minute, 500*time.Millisecond, "the probes' results")

	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "probed"})
	require.NoError(t, err)
	for _, p := range inspected.Machine.Probes {
		if p.Name == "exec-bad" || p.Name == "closed" || p.Name == "http-bad" || p.Name == "http-missing" {
			assert.NotEmpty(t, p.LastFailure, "a failing probe says why: %s", p.Name)
			assert.GreaterOrEqual(t, p.ConsecutiveFailures, uint32(1))
		}
	}

	// Every probe's change reached the event stream, the last one of each being the status
	// Inspect reported. A probe may fail once before the entrypoint listens, so an earlier event
	// of a probe need not be its final status.
	seen := map[string]v1alpha2.ProbeStatus{}
	deadline := time.After(time.Minute)
	reached := func() bool {
		for name, status := range want {
			if seen[name] != status {
				return false
			}
		}
		return true
	}
	for !reached() {
		select {
		case r := <-changes:
			seen[r.Name] = r.Status
		case <-deadline:
			t.Fatalf("the probe events of a minute: %v", seen)
		}
	}

	// A Stopped Machine reports each probe Unknown.
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "probed", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	for name, status := range n.probeStatuses(t, ctx, "probed") {
		assert.Equal(t, v1alpha2.ProbeStatus_PROBE_STATUS_UNKNOWN, status, name)
	}

	// A probe the agent would refuse is refused at Create, naming it.
	bad := createRequest("probed-bad", pulled.Digest)
	bad.Probes = []*v1alpha2.Probe{tcpProbe("no-port", 0)}
	_, err = n.client.CreateMachine(ctx, bad)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no-port")
}
