package events

import "github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"

// Noop is an Emitter that discards every call. Tests use it when they do not
// expect events.
type Noop struct{}

var _ Emitter = Noop{}

func (Noop) EmitStateChanged(string, v1alpha1.MachineState) {}
func (Noop) EmitDeadline(v1alpha1.DeadlineEvent_Kind)       {}
func (Noop) EmitDriver(string, string)                      {}
