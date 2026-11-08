package machine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The tap and the guest's resources are the Machine's, so a Driver's own
// statement of them is replaced; what else the Driver described is kept.
func TestProcessDescriptionFor_TakesTheTapAndTheGuestFromTheBoot(t *testing.T) {
	described := machine.ProcessDescription{
		Executable:      "/vmm",
		TapByDescriptor: true,
		Overhead:        machine.ProcessOverhead{Tasks: 7},
		Tap:             "the Driver's own claim",
		Guest:           machine.Resources{VCPUs: 99, MemoryBytes: 99},
	}
	boot := machine.BootSpec{
		Network:   machine.NetworkDevice{Tap: "tap0"},
		Resources: machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30},
	}

	got := described.For(boot)

	assert.Equal(t, "tap0", got.Tap)
	assert.Equal(t, machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30}, got.Guest)
	assert.Equal(t, "/vmm", got.Executable, "control: the rest of the description is kept")
	assert.True(t, got.TapByDescriptor)
	assert.Equal(t, uint32(7), got.Overhead.Tasks)
	assert.Equal(t, "the Driver's own claim", described.Tap, "the description it was called on is not changed")
}

// The run directory is the lifecycle's too: a Driver cannot send the
// supervisor to another directory than the one it wrote its sockets in.
func TestProcessDescriptionFor_TakesTheRunDirectoryFromTheBoot(t *testing.T) {
	got := machine.ProcessDescription{Executable: "/vmm", RunDir: "/the/Driver's/own"}.For(machine.BootSpec{RunDir: "/run/m"})

	assert.Equal(t, "/run/m", got.RunDir)
	assert.Equal(t, "/vmm", got.Executable, "control: the rest of the description is kept")
}
