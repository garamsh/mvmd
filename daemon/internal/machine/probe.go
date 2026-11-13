package machine

import "time"

// The limits the Guest Agent holds a Machine's probes to (ADR 0054): a request
// outside them is refused at Create, naming the probe.
const (
	maxProbes        = 8
	maxProbeName     = 63
	maxProbePath     = 2048
	minProbeInterval = time.Millisecond
	maxProbeDuration = 24 * time.Hour
)

// ProbeKind is what a probe checks.
type ProbeKind int

const (
	// ProbeExec runs Argv in the guest and succeeds on exit code 0.
	ProbeExec ProbeKind = iota + 1
	// ProbeTCP connects to Port on the guest's loopback address.
	ProbeTCP
	// ProbeHTTPGet gets Path from Port on the guest's loopback address and
	// succeeds on a status from 200 to 399.
	ProbeHTTPGet
)

// Probe is a check of the workload the guest runs on a schedule from the
// entrypoint's start. A zero Period, Timeout or threshold takes the agent's
// default and is sent as unset, so that the Machine's record holds what the
// request said.
type Probe struct {
	Name string
	Kind ProbeKind
	// Argv is the program and its arguments of a ProbeExec.
	Argv []string
	// Port is the port of a ProbeTCP or ProbeHTTPGet.
	Port uint32
	// Path and Scheme are those of a ProbeHTTPGet; an empty Scheme is "http".
	Path   string
	Scheme string
	// InitialDelay is zero for none, which is also the default. Period and
	// Timeout are zero when the request set none.
	InitialDelay     time.Duration
	Period           time.Duration
	Timeout          time.Duration
	SuccessThreshold uint32
	FailureThreshold uint32
}

// ProbeStatus is where a probe is.
type ProbeStatus int

const (
	// ProbeUnknown is a probe that has not met a threshold, or one of which the
	// node holds no result.
	ProbeUnknown ProbeStatus = iota
	ProbePassing
	ProbeFailing
)

// ProbeResult is what the guest last sent of one probe.
type ProbeResult struct {
	Name   string
	Status ProbeStatus
	// ChangedAt is the guest's clock when Status last changed; zero while
	// Unknown.
	ChangedAt            time.Time
	ConsecutiveSuccesses uint32
	ConsecutiveFailures  uint32
	// LastFailure is why the most recent failed run failed, as text for
	// people. It never holds output of the workload.
	LastFailure string
	// LastOutput is the first 1 KiB of an exec probe's most recent run.
	LastOutput []byte
}

func (r ProbeResult) clone() ProbeResult {
	r.LastOutput = cloneNonEmpty(r.LastOutput)
	return r
}

// same reports whether r holds every field the node reports as o does.
func (r ProbeResult) same(o ProbeResult) bool {
	return r.Name == o.Name && r.Status == o.Status && r.ChangedAt.Equal(o.ChangedAt) &&
		r.ConsecutiveSuccesses == o.ConsecutiveSuccesses && r.ConsecutiveFailures == o.ConsecutiveFailures &&
		r.LastFailure == o.LastFailure && string(r.LastOutput) == string(o.LastOutput)
}

func cloneProbes(in []Probe) []Probe {
	out := cloneNonEmpty(in)
	for i := range out {
		out[i].Argv = cloneNonEmpty(out[i].Argv)
	}
	return out
}

// validateProbes refuses the probes the guest would refuse, so a bad probe
// never reaches a boot. A message names the probe.
func validateProbes(probes []Probe) error {
	if len(probes) > maxProbes {
		return fail(ErrInvalid, "probes: %d probes, at most %d", len(probes), maxProbes)
	}
	names := make(map[string]bool, len(probes))
	for _, p := range probes {
		if err := p.validate(); err != nil {
			return err
		}
		if names[p.Name] {
			return fail(ErrInvalid, "probes: probe %q is given twice", p.Name)
		}
		names[p.Name] = true
	}
	return nil
}

func (p Probe) validate() error {
	if p.Name == "" || len(p.Name) > maxProbeName || !probeNameBytes(p.Name) {
		return fail(ErrInvalid, "probes: probe %q has a name that is not 1 to %d letters, digits, '-', '_' or '.'", p.Name, maxProbeName)
	}
	switch p.Kind {
	case ProbeExec:
		if len(p.Argv) == 0 || p.Argv[0] == "" {
			return fail(ErrInvalid, "probes: probe %q has an exec argv with no program", p.Name)
		}
	case ProbeTCP:
		if p.Port == 0 || p.Port > 65535 {
			return fail(ErrInvalid, "probes: probe %q has port %d, not from 1 to 65535", p.Name, p.Port)
		}
	case ProbeHTTPGet:
		if p.Scheme != "" && p.Scheme != "http" {
			return fail(ErrInvalid, "probes: probe %q has scheme %q, only \"http\" is spoken", p.Name, p.Scheme)
		}
		if p.Path == "" || p.Path[0] != '/' || len(p.Path) > maxProbePath || !printableASCII(p.Path) {
			return fail(ErrInvalid, "probes: probe %q has a path that does not begin with '/', or is over %d bytes, or holds a space, a control or a non-ASCII character", p.Name, maxProbePath)
		}
		if p.Port == 0 || p.Port > 65535 {
			return fail(ErrInvalid, "probes: probe %q has port %d, not from 1 to 65535", p.Name, p.Port)
		}
	default:
		return fail(ErrInvalid, "probes: probe %q has no kind", p.Name)
	}
	for _, d := range []struct {
		field string
		value time.Duration
		least time.Duration
		set   bool
	}{
		{"initial_delay", p.InitialDelay, 0, true},
		{"period", p.Period, minProbeInterval, p.Period != 0},
		{"timeout", p.Timeout, minProbeInterval, p.Timeout != 0},
	} {
		if d.set && (d.value < d.least || d.value > maxProbeDuration) {
			return fail(ErrInvalid, "probes: probe %q has %s %v, not from %v to %v", p.Name, d.field, d.value, d.least, maxProbeDuration)
		}
	}
	return nil
}

func probeNameBytes(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

func printableASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] <= ' ' || s[i] >= 0x7f {
			return false
		}
	}
	return true
}

// unknownProbes is one Unknown result for each probe, in order.
func unknownProbes(probes []Probe) []ProbeResult {
	out := make([]ProbeResult, len(probes))
	for i, p := range probes {
		out[i] = ProbeResult{Name: p.Name}
	}
	return out
}
