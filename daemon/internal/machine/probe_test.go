package machine_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// probeWait bounds every wait of a test below on the follower, which runs on
// its own goroutine.
const probeWait = 5 * time.Second

var (
	execProbe = machine.Probe{Name: "ready", Kind: machine.ProbeExec, Argv: []string{"/bin/true"}}
	tcpProbe  = machine.Probe{Name: "port", Kind: machine.ProbeTCP, Port: 8080}
	httpProbe = machine.Probe{Name: "web", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/healthz"}
)

func passing(name string) machine.ProbeResult {
	return machine.ProbeResult{Name: name, Status: machine.ProbePassing, ConsecutiveSuccesses: 1}
}

func failing(name string) machine.ProbeResult {
	return machine.ProbeResult{Name: name, Status: machine.ProbeFailing, ConsecutiveFailures: 3, LastFailure: "exit status 1"}
}

// withProbes is an edit that gives the Machine its probes.
func withProbes(p ...machine.Probe) func(*machine.Spec) {
	return func(s *machine.Spec) { s.Probes = p }
}

// requireProbeStatuses waits, bounded, until Inspect reports the statuses of
// the Machine's probes in order, and returns what it then reported.
func requireProbeStatuses(t *testing.T, svc machine.Service, id machine.MachineID, want ...machine.ProbeStatus) []machine.ProbeResult {
	t.Helper()
	var got []machine.ProbeResult
	require.Eventually(t, func() bool {
		m, err := svc.Inspect(context.Background(), id)
		require.NoError(t, err)
		got = m.ProbeResults
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i].Status != want[i] {
				return false
			}
		}
		return true
	}, probeWait, time.Millisecond, "probe statuses of %q, last read %+v", id, got)
	return got
}

// requireProbeEvents waits, bounded, until the Machine has been reported to
// have exactly the results, in order.
func (e *env) requireProbeEvents(t *testing.T, id machine.MachineID, want ...machine.ProbeResult) {
	t.Helper()
	require.Eventually(t, func() bool { return len(e.events.ProbeChanges(id)) >= len(want) }, probeWait, time.Millisecond)
	assert.Equal(t, want, e.events.ProbeChanges(id))
}

// A probe the agent would refuse is refused at Create, naming the probe.
func TestCreate_AProbeTheAgentWouldRefuseIsRefusedNamingIt(t *testing.T) {
	named := func(p machine.Probe, name string) machine.Probe { p.Name = name; return p }
	many := make([]machine.Probe, 9)
	for i := range many {
		many[i] = named(tcpProbe, strings.Repeat("p", i+1))
	}
	tests := []struct {
		name   string
		probes []machine.Probe
		want   string
	}{
		{"nine probes", many, "probes"},
		{"an empty name", []machine.Probe{named(tcpProbe, "")}, `""`},
		{"a name of 64 bytes", []machine.Probe{named(tcpProbe, strings.Repeat("n", 64))}, strings.Repeat("n", 64)},
		{"a name with a space", []machine.Probe{named(tcpProbe, "a b")}, `"a b"`},
		{"a repeated name", []machine.Probe{execProbe, named(tcpProbe, "ready")}, `"ready"`},
		{"no kind", []machine.Probe{{Name: "none"}}, `"none"`},
		{"an exec probe with no argv", []machine.Probe{{Name: "e", Kind: machine.ProbeExec}}, `"e"`},
		{"an exec probe with an empty program", []machine.Probe{{Name: "e", Kind: machine.ProbeExec, Argv: []string{""}}}, `"e"`},
		{"tcp port 0", []machine.Probe{{Name: "t", Kind: machine.ProbeTCP}}, `"t"`},
		{"tcp port 65536", []machine.Probe{{Name: "t", Kind: machine.ProbeTCP, Port: 65536}}, `"t"`},
		{"http port 0", []machine.Probe{{Name: "h", Kind: machine.ProbeHTTPGet, Path: "/"}}, `"h"`},
		{"a path that does not begin with a slash", []machine.Probe{{Name: "h", Kind: machine.ProbeHTTPGet, Port: 80, Path: "healthz"}}, `"h"`},
		{"a path with a space", []machine.Probe{{Name: "h", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/a b"}}, `"h"`},
		{"a path of 2049 bytes", []machine.Probe{{Name: "h", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/" + strings.Repeat("a", 2048)}}, `"h"`},
		{"https", []machine.Probe{{Name: "h", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/", Scheme: "https"}}, `"h"`},
		{"a period over 24h", []machine.Probe{{Name: "p", Kind: machine.ProbeTCP, Port: 1, Period: 24*time.Hour + time.Second}}, `"p"`},
		{"a timeout under 1ms", []machine.Probe{{Name: "p", Kind: machine.ProbeTCP, Port: 1, Timeout: time.Microsecond}}, `"p"`},
		{"a period under 1ms", []machine.Probe{{Name: "p", Kind: machine.ProbeTCP, Port: 1, Period: time.Microsecond}}, `"p"`},
		{"an initial delay over 24h", []machine.Probe{{Name: "p", Kind: machine.ProbeTCP, Port: 1, InitialDelay: 25 * time.Hour}}, `"p"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			spec := baseSpec()
			spec.Probes = tt.probes
			err := e.svc.Create(context.Background(), "m", "r", spec)
			requireKind(t, err, machine.ErrInvalid)
			assert.Contains(t, err.Error(), tt.want)
			e.requireNoMachine(t, "m")
		})
	}

	// Control: eight probes of every kind, with every limit at its edge, are accepted.
	e := newEnv()
	spec := baseSpec()
	spec.Probes = []machine.Probe{
		execProbe, tcpProbe, httpProbe,
		{Name: strings.Repeat("n", 63), Kind: machine.ProbeTCP, Port: 65535, Period: time.Millisecond, Timeout: time.Millisecond, InitialDelay: 24 * time.Hour},
		{Name: "a.b_c-D9", Kind: machine.ProbeHTTPGet, Port: 1, Path: "/" + strings.Repeat("a", 2047), Scheme: "http", Period: 24 * time.Hour},
		named(tcpProbe, "p6"), named(tcpProbe, "p7"), named(tcpProbe, "p8"),
	}
	require.NoError(t, e.svc.Create(context.Background(), "m", "r", spec))
}

// A probe is part of what Create was asked for: the same request repeats, and
// other probes are a conflict.
func TestCreate_RepeatKeyIncludesTheProbes(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	spec := baseSpec()
	spec.Probes = []machine.Probe{execProbe, httpProbe}
	require.NoError(t, e.svc.Create(ctx, "m", "r", spec))

	same := baseSpec()
	same.Probes = []machine.Probe{execProbe, httpProbe}
	require.NoError(t, e.svc.Create(ctx, "m", "r", same), "the same probes are the same call")
	assert.Equal(t, 1, e.disks.Calls("create-root"))

	for name, probes := range map[string][]machine.Probe{
		"none":            nil,
		"one fewer":       {execProbe},
		"another order":   {httpProbe, execProbe},
		"another period":  {execProbe, {Name: "web", Kind: machine.ProbeHTTPGet, Port: 80, Path: "/healthz", Period: time.Second}},
		"another command": {{Name: "ready", Kind: machine.ProbeExec, Argv: []string{"/bin/false"}}, httpProbe},
	} {
		t.Run(name, func(t *testing.T) {
			other := baseSpec()
			other.Probes = probes
			requireKind(t, e.svc.Create(ctx, "m", "r", other), machine.ErrConflict)
			assert.Equal(t, 1, e.disks.Calls("create-root"))
		})
	}
}

// The daemon opens the guest's stream once the Machine is booted, and reports
// what the guest sent.
func TestProbes_AreFollowedFromBoot(t *testing.T) {
	e := newEnv()
	boot := e.startedWith(t, "m", image, withProbes(execProbe, tcpProbe))
	assert.Equal(t, []machine.Probe{execProbe, tcpProbe}, boot.Probes, "the guest is sent them at boot")

	// Before the guest sends anything every probe is Unknown.
	requireProbeStatuses(t, e.svc, "m", machine.ProbeUnknown, machine.ProbeUnknown)

	e.guest.SetProbeResult("m", passing("port"))
	got := requireProbeStatuses(t, e.svc, "m", machine.ProbeUnknown, machine.ProbePassing)
	assert.Equal(t, []string{"ready", "port"}, []string{got[0].Name, got[1].Name}, "in the order they were created with")
	assert.Equal(t, uint32(1), got[1].ConsecutiveSuccesses)
	assert.Equal(t, 1, e.guest.ProbeOpens("m"))

	// Control: a Machine with no probes opens no stream.
	e.startedWith(t, "plain", image, func(*machine.Spec) {})
	assert.Zero(t, e.guest.ProbeOpens("plain"))
	m, err := e.svc.Inspect(context.Background(), "plain")
	require.NoError(t, err)
	assert.Empty(t, m.ProbeResults)
}

// A Machine restored from a snapshot is followed once it has caught up, and
// its probes are the snapshot's.
func TestProbes_AreFollowedAfterARestore(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshottedWith(t, "src", withProbes(execProbe, httpProbe))
	got, _, err := e.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	require.Equal(t, []machine.Probe{execProbe, httpProbe}, got.Spec.Probes, "the setup: the snapshot kept them")

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	e.requireState(t, "new", machine.StateRunning)
	e.guest.SetProbeResult("new", failing("web"))
	requireProbeStatuses(t, e.svc, "new", machine.ProbeUnknown, machine.ProbeFailing)
	assert.Equal(t, 1, e.guest.ProbeOpens("new"))
}

// A daemon that finds a Machine Running opens the stream again, and reads back
// what it lost.
func TestProbes_AreFollowedAgainAfterARestart(t *testing.T) {
	e := newEnv()
	e.startedWith(t, "m", image, withProbes(execProbe))
	e.guest.SetProbeResult("m", passing("ready"))
	requireProbeStatuses(t, e.svc, "m", machine.ProbePassing)
	require.Equal(t, 1, e.guest.ProbeOpens("m"))

	svc := e.reopened(t)
	require.Eventually(t, func() bool { return e.guest.ProbeOpens("m") == 2 }, probeWait, time.Millisecond, "the new daemon opened the stream")
	got := requireProbeStatuses(t, svc, "m", machine.ProbePassing)
	assert.Equal(t, uint32(1), got[0].ConsecutiveSuccesses, "the guest's current result was read on open")
}

// A stream that broke is opened again.
func TestProbes_AStreamThatBrokeIsOpenedAgain(t *testing.T) {
	e := newEnv()
	e.startedWith(t, "m", image, withProbes(execProbe))
	require.Eventually(t, func() bool { return e.guest.ProbeStreams("m") == 1 }, probeWait, time.Millisecond)
	e.guest.BreakProbes("m")
	require.Eventually(t, func() bool { return e.guest.ProbeOpens("m") == 2 }, probeWait, time.Millisecond, "opened again")
	e.guest.SetProbeResult("m", passing("ready"))
	requireProbeStatuses(t, e.svc, "m", machine.ProbePassing)
}

// A change of a probe's result is one event; the guest repeating what the
// daemon holds is none.
func TestProbes_OnlyAChangeIsAnEvent(t *testing.T) {
	e := newEnv()
	e.startedWith(t, "m", image, withProbes(execProbe, tcpProbe))
	require.Eventually(t, func() bool { return e.guest.ProbeStreams("m") == 1 }, probeWait, time.Millisecond)

	e.guest.SetProbeResult("m", passing("ready"))
	e.guest.SetProbeResult("m", passing("ready")) // unchanged: no event
	e.guest.SetProbeResult("m", passing("ready")) // unchanged: no event
	// Control: a changed result does emit. Results arrive in order, so the
	// event of the failing result follows every one the guest repeated.
	e.guest.SetProbeResult("m", failing("ready"))
	e.guest.SetProbeResult("m", passing("port"))
	e.requireProbeEvents(t, "m", passing("ready"), failing("ready"), passing("port"))
}

// What the guest sends for a name the Machine has no probe of is not held.
func TestProbes_AResultOfAnotherNameIsNotHeld(t *testing.T) {
	e := newEnv()
	e.startedWith(t, "m", image, withProbes(execProbe))
	require.Eventually(t, func() bool { return e.guest.ProbeStreams("m") == 1 }, probeWait, time.Millisecond)
	e.guest.SetProbeResult("m", passing("stranger"))
	e.guest.SetProbeResult("m", passing("ready"))
	e.requireProbeEvents(t, "m", passing("ready"))
}

// A Paused Machine reports the results held; a Stopped one reports Unknown and
// no event says so; a started one is read again from the guest.
func TestProbes_ByState(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.startedWith(t, "m", image, withProbes(execProbe))
	e.guest.SetProbeResult("m", passing("ready"))
	requireProbeStatuses(t, e.svc, "m", machine.ProbePassing)

	require.NoError(t, e.svc.Pause(ctx, "m"))
	m := e.requireState(t, "m", machine.StatePaused)
	require.Len(t, m.ProbeResults, 1)
	assert.Equal(t, machine.ProbePassing, m.ProbeResults[0].Status, "Paused reports what was held")
	listed, err := e.svc.List(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, machine.ProbePassing, listed[0].ProbeResults[0].Status, "List too")

	require.NoError(t, e.svc.Resume(ctx, "m"))
	requireProbeStatuses(t, e.svc, "m", machine.ProbePassing)
	assert.Equal(t, 1, e.guest.ProbeOpens("m"), "a Pause and a Resume do not open the stream again")

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	m = e.requireState(t, "m", machine.StateStopped)
	require.Len(t, m.ProbeResults, 1)
	assert.Equal(t, "ready", m.ProbeResults[0].Name)
	assert.Equal(t, machine.ProbeUnknown, m.ProbeResults[0].Status, "Stopped reports Unknown")
	assert.Equal(t, []machine.ProbeResult{passing("ready")}, e.events.ProbeChanges("m"), "no per-probe event says it")
	require.Eventually(t, func() bool { return e.guest.ProbeStreams("m") == 0 }, probeWait, time.Millisecond, "the stream was closed")

	// The guest of the next boot starts over, and so does the daemon.
	require.NoError(t, e.svc.Start(ctx, "m"))
	requireProbeStatuses(t, e.svc, "m", machine.ProbePassing)
	assert.Equal(t, 2, e.guest.ProbeOpens("m"))
}

// A Machine that is not Running or Paused and had its probes reports each
// Unknown.
func TestProbes_ACreatedMachineReportsEachUnknown(t *testing.T) {
	e := newEnv()
	spec := baseSpec()
	spec.Probes = []machine.Probe{execProbe, tcpProbe}
	require.NoError(t, e.svc.Create(context.Background(), "m", "r", spec))
	m := e.requireState(t, "m", machine.StateCreated)
	require.Len(t, m.ProbeResults, 2)
	assert.Equal(t, "ready", m.ProbeResults[0].Name)
	assert.Equal(t, "port", m.ProbeResults[1].Name)
	assert.Equal(t, machine.ProbeUnknown, m.ProbeResults[0].Status)
	assert.Equal(t, machine.ProbeUnknown, m.ProbeResults[1].Status)
}

// A run update when the Machine is stopped does not change the probes.
func TestProbes_ARecordKeepsThemAcrossAStop(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.startedWith(t, "m", image, withProbes(execProbe, httpProbe))
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.Start(ctx, "m"))
	boots := e.guest.Boots("m")
	require.Len(t, boots, 2)
	assert.Equal(t, []machine.Probe{execProbe, httpProbe}, boots[1].Probes)
	rec, ok := e.store.Record("m")
	require.True(t, ok)
	assert.Equal(t, []machine.Probe{execProbe, httpProbe}, rec.Machine.Probes)
}
