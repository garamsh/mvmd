package api

import (
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

var probeStatuses = map[machine.ProbeStatus]v1alpha2.ProbeStatus{
	machine.ProbeUnknown: v1alpha2.ProbeStatus_PROBE_STATUS_UNKNOWN,
	machine.ProbePassing: v1alpha2.ProbeStatus_PROBE_STATUS_PASSING,
	machine.ProbeFailing: v1alpha2.ProbeStatus_PROBE_STATUS_FAILING,
}

// durationOf is the domain form of a wire duration: zero where it is unset.
func durationOf(d *durationpb.Duration) time.Duration {
	if d == nil {
		return 0
	}
	return d.AsDuration()
}

func probesOf(in []*v1alpha2.Probe) []machine.Probe {
	var out []machine.Probe
	for _, p := range in {
		probe := machine.Probe{
			Name:             p.GetName(),
			InitialDelay:     durationOf(p.GetInitialDelay()),
			Period:           durationOf(p.GetPeriod()),
			Timeout:          durationOf(p.GetTimeout()),
			SuccessThreshold: p.GetSuccessThreshold(),
			FailureThreshold: p.GetFailureThreshold(),
		}
		switch kind := p.GetKind().(type) {
		case *v1alpha2.Probe_Exec:
			probe.Kind, probe.Argv = machine.ProbeExec, kind.Exec.GetArgv()
		case *v1alpha2.Probe_Tcp:
			probe.Kind, probe.Port = machine.ProbeTCP, kind.Tcp.GetPort()
		case *v1alpha2.Probe_HttpGet:
			probe.Kind, probe.Port, probe.Path, probe.Scheme = machine.ProbeHTTPGet, kind.HttpGet.GetPort(), kind.HttpGet.GetPath(), kind.HttpGet.GetScheme()
		}
		out = append(out, probe)
	}
	return out
}

func probeResultToWire(r machine.ProbeResult) *v1alpha2.ProbeResult {
	return &v1alpha2.ProbeResult{
		Name:                 r.Name,
		Status:               probeStatuses[r.Status],
		ChangedAt:            timeToWire(r.ChangedAt),
		ConsecutiveSuccesses: r.ConsecutiveSuccesses,
		ConsecutiveFailures:  r.ConsecutiveFailures,
		LastFailure:          r.LastFailure,
		LastOutput:           r.LastOutput,
	}
}

func probeResultsToWire(in []machine.ProbeResult) []*v1alpha2.ProbeResult {
	var out []*v1alpha2.ProbeResult
	for _, r := range in {
		out = append(out, probeResultToWire(r))
	}
	return out
}
