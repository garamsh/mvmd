package firecracker_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Firecracker v1.17.0 documents PATCH /drives/{id} only for a device the guest
// has not mounted, and it does not grow the host file, so the Driver refuses a
// live disk growth and does not report the capability (ADR 0055).
func TestResizeDisk_IsRefusedAndTheCapabilityIsNotReported(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	err := n.driver.ResizeDisk(context.Background(), "m1", "data", 8<<20)

	require.ErrorIs(t, err, machine.ErrUnsupported)
	assert.Contains(t, err.Error(), "patch-block.md")
	assert.Empty(t, since(f, before), "the VMM was asked nothing")
	assert.NotContains(t, n.driver.Info().Capabilities, machine.CapabilityDiskResize)
	assert.Contains(t, n.driver.Info().Capabilities, machine.CapabilityDiskHotplug, "control: what it can do is reported")
}
