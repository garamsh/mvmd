package guestchannel

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"

	guestv1alpha1 "github.com/garamsh/mvmd/daemon/internal/guestapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// runProbes is the Machine's probes as the agent takes them. A duration or
// threshold the Machine holds as zero is sent unset, so that the agent applies
// its own defaults (ADR 0054).
func runProbes(probes []machine.Probe) []*guestv1alpha1.Probe {
	var out []*guestv1alpha1.Probe
	for _, p := range probes {
		probe := &guestv1alpha1.Probe{
			Name:             p.Name,
			InitialDelay:     protoDuration(p.InitialDelay),
			Period:           protoDuration(p.Period),
			Timeout:          protoDuration(p.Timeout),
			SuccessThreshold: p.SuccessThreshold,
			FailureThreshold: p.FailureThreshold,
		}
		switch p.Kind {
		case machine.ProbeExec:
			probe.Kind = &guestv1alpha1.Probe_Exec{Exec: &guestv1alpha1.ExecProbe{Argv: p.Argv}}
		case machine.ProbeTCP:
			probe.Kind = &guestv1alpha1.Probe_Tcp{Tcp: &guestv1alpha1.TcpProbe{Port: p.Port}}
		case machine.ProbeHTTPGet:
			probe.Kind = &guestv1alpha1.Probe_HttpGet{HttpGet: &guestv1alpha1.HttpGetProbe{Port: p.Port, Path: p.Path, Scheme: p.Scheme}}
		}
		out = append(out, probe)
	}
	return out
}

func protoDuration(d time.Duration) *durationpb.Duration {
	if d == 0 {
		return nil
	}
	return durationpb.New(d)
}

func probeResult(r *guestv1alpha1.ProbeResult) (machine.ProbeResult, error) {
	var s machine.ProbeStatus
	switch r.GetStatus() {
	case guestv1alpha1.ProbeStatus_PROBE_STATUS_UNKNOWN:
		s = machine.ProbeUnknown
	case guestv1alpha1.ProbeStatus_PROBE_STATUS_PASSING:
		s = machine.ProbePassing
	case guestv1alpha1.ProbeStatus_PROBE_STATUS_FAILING:
		s = machine.ProbeFailing
	default:
		return machine.ProbeResult{}, fmt.Errorf("the guest agent sent probe status %v, which this daemon does not know", r.GetStatus())
	}
	var changed time.Time
	if at := r.GetChangedAt(); at != nil {
		changed = at.AsTime()
	}
	return machine.ProbeResult{
		Name:                 r.GetName(),
		Status:               s,
		ChangedAt:            changed,
		ConsecutiveSuccesses: r.GetConsecutiveSuccesses(),
		ConsecutiveFailures:  r.GetConsecutiveFailures(),
		LastFailure:          r.GetLastFailure(),
		LastOutput:           r.GetLastOutput(),
	}, nil
}

// WatchProbes implements machine.Guest. The agent ends the stream after the
// entrypoint has ended, which is a clean end and returns nil; a stream that
// broke is an error, and the caller opens it again, since the agent sends the
// current results on open.
func (g *Guest) WatchProbes(ctx context.Context, id machine.MachineID, send func(machine.ProbeResult) error) error {
	return g.run(ctx, id, opProbes, func(c guestv1alpha1.GuestServiceClient) error {
		stream, err := c.WatchProbes(ctx, &guestv1alpha1.WatchProbesRequest{})
		if err != nil {
			return err
		}
		for {
			msg, err := stream.Recv()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			result, err := probeResult(msg.GetResult())
			if err != nil {
				return err
			}
			if err := send(result); err != nil {
				return err
			}
		}
	})
}
