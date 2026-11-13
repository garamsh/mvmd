package memory

import (
	"context"
	"errors"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// probeFeed is what the guest of one Machine holds of its probes, and the
// streams open on them.
type probeFeed struct {
	results map[string]machine.ProbeResult
	order   []string
	opens   int
	streams map[*probeStream]bool
}

// probeStream is one open WatchProbes. changes carries what is sent to it
// after the results sent on open; end carries how the stream is to end.
type probeStream struct {
	changes chan machine.ProbeResult
	end     chan error
}

// probeFeedBuffer is how many changes a stream holds before it drops the
// stream, as the real agent's buffer does not model: a test that sets more
// without a reader is a mistake it should hear of.
const probeFeedBuffer = 1024

func (g *Guest) feed(id machine.MachineID) *probeFeed {
	f := g.probeFeeds[id]
	if f == nil {
		f = &probeFeed{results: make(map[string]machine.ProbeResult), streams: make(map[*probeStream]bool)}
		g.probeFeeds[id] = f
	}
	return f
}

// current is what a stream sends on open: the result of every probe of the
// last boot description in its order, or, where the guest took none, of every
// probe it holds a result of, in the order they were first set.
func (g *Guest) current(id machine.MachineID, f *probeFeed) []machine.ProbeResult {
	var names []string
	if boots := g.boots[id]; len(boots) > 0 {
		for _, p := range boots[len(boots)-1].Probes {
			names = append(names, p.Name)
		}
	} else {
		names = f.order
	}
	out := make([]machine.ProbeResult, 0, len(names))
	for _, name := range names {
		r, ok := f.results[name]
		if !ok {
			r = machine.ProbeResult{Name: name}
		}
		out = append(out, r)
	}
	return out
}

// SetProbeResult makes the result the guest's current one of its probe, and
// sends it to every stream open on the Machine.
func (g *Guest) SetProbeResult(id machine.MachineID, result machine.ProbeResult) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f := g.feed(id)
	if _, ok := f.results[result.Name]; !ok {
		f.order = append(f.order, result.Name)
	}
	f.results[result.Name] = result
	for s := range f.streams {
		select {
		case s.changes <- result:
		default:
			panic("memory.Guest: a probe stream is not read")
		}
	}
}

// EndProbes ends every stream open on the Machine as the agent does after the
// entrypoint has ended: with no error.
func (g *Guest) EndProbes(id machine.MachineID) { g.endProbes(id, nil) }

// BreakProbes ends every stream open on the Machine with an error, as a
// channel that broke does.
func (g *Guest) BreakProbes(id machine.MachineID) {
	g.endProbes(id, errors.Join(machine.ErrGuestUnreachable, errors.New("the stream broke")))
}

func (g *Guest) endProbes(id machine.MachineID, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for s := range g.feed(id).streams {
		select {
		case s.end <- err:
		default:
		}
	}
}

// ProbeOpens returns how many times WatchProbes was opened on the Machine.
func (g *Guest) ProbeOpens(id machine.MachineID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.feed(id).opens
}

// ProbeStreams returns how many streams are open on the Machine now.
func (g *Guest) ProbeStreams(id machine.MachineID) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.feed(id).streams)
}

// WatchProbes implements machine.Guest. Its step is "watch-probes".
func (g *Guest) WatchProbes(ctx context.Context, id machine.MachineID, send func(machine.ProbeResult) error) error {
	if err := g.enter("watch-probes"); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return errors.Join(machine.ErrGuestUnreachable, err)
	}
	stream := &probeStream{changes: make(chan machine.ProbeResult, probeFeedBuffer), end: make(chan error, 1)}
	g.mu.Lock()
	f := g.feed(id)
	f.opens++
	f.streams[stream] = true
	initial := g.current(id, f)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		delete(f.streams, stream)
		g.mu.Unlock()
	}()
	for _, r := range initial {
		if err := send(r); err != nil {
			return err
		}
	}
	for {
		select {
		case r := <-stream.changes:
			if err := send(r); err != nil {
				return err
			}
		case err := <-stream.end:
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
