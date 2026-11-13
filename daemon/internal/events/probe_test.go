package events_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A probe's new result is an event of its Machine, in the node's one sequence.
func TestWatch_AProbeChangeIsAnEventOfItsMachine(t *testing.T) {
	s := events.NewService(10)
	s.StateChanged("a", machine.StateRunning)
	result := machine.ProbeResult{Name: "ready", Status: machine.ProbeFailing, ConsecutiveFailures: 3, LastFailure: "exit status 1"}
	s.ProbeChanged("a", result)

	w := s.Watch(0)
	assert.Equal(t, events.StateChanged{State: machine.StateRunning}, next(t, w).Body)
	e := next(t, w)
	assert.Equal(t, uint64(2), e.Sequence)
	assert.Equal(t, machine.MachineID("a"), e.Machine)
	assert.Equal(t, events.ProbeChanged{Result: result}, e.Body)
}
