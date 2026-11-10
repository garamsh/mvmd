package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ADR 0029: after the Driver has resized the VMM, the guest brings the new
// vCPUs online, as it finds a disk after the Driver has plugged it.
func TestSetResources_AfterTheDriverResizesTheGuestOnlinesTheTargetsVCPUs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)

	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))

	assert.Equal(t, 1, e.driver.Calls("resize"))
	assert.Equal(t, 1, e.guest.Calls("online-vcpus"), "the guest was asked once the VMM had the vCPUs")
	assert.Equal(t, large, e.requireState(t, "m", machine.StateRunning).Resources)
}

func TestSetResources_AMachineNotRunningIsNoGuestCall(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateStopped)

	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))

	assert.Zero(t, e.guest.Calls("online-vcpus"), "a Machine with no guest has nothing to online")
	assert.Zero(t, e.driver.Calls("resize"))
}

// The VMM has presented the vCPUs when its resize returns, and the guest sees
// them a moment later; the call waits until the count is the target's.
func TestSetResources_WaitsUntilTheGuestReportsTheTargetsVCPUs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	e.guest.PresentVcpus("m", 2)
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.guest.PresentVcpus("m", 4)
	}()

	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))

	assert.Greater(t, e.guest.Calls("online-vcpus"), 1, "asked again until it reported four")
	assert.Equal(t, large, e.requireState(t, "m", machine.StateRunning).Resources)
}

func TestSetResources_AGuestThatNeverReportsTheTargetFailsTheResize(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	e.guest.PresentVcpus("m", 2)
	e.svc = e.reopenedWith(t, machine.WithResizeTimeout(300*time.Millisecond))

	err := e.svc.SetResources(ctx, "m", large, machine.Resources{})

	requireKind(t, err, machine.ErrFailed)
	assert.Equal(t, small, e.requireState(t, "m", machine.StateRunning).Resources, "the record keeps what the Machine was recorded with")

	e.guest.PresentVcpus("m", 4)
	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), "control: the retry converges once the guest sees them")
	assert.Equal(t, large, e.requireState(t, "m", machine.StateRunning).Resources)
	assert.Equal(t, 2, e.driver.Calls("resize"), "the Driver was asked again and sends only what differs")
}

func TestSetResources_AGuestFailureFailsTheResizeAndARetryConverges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	e.guest.FailAt("online-vcpus", errBoom)

	err := e.svc.SetResources(ctx, "m", large, machine.Resources{})

	requireKind(t, err, machine.ErrFailed)
	assert.Equal(t, small, e.requireState(t, "m", machine.StateRunning).Resources)

	e.guest.Heal("online-vcpus")
	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))
	assert.Equal(t, large, e.requireState(t, "m", machine.StateRunning).Resources)
}

func TestSetResources_ADriverFailureAsksTheGuestNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	e.driver.FailAt("resize", errBoom)

	requireKind(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), machine.ErrFailed)
	assert.Zero(t, e.guest.Calls("online-vcpus"), "the VMM has no new vCPUs to online")

	e.driver.Heal("resize")
	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), "control: healed, the same call resizes")
	assert.Equal(t, 1, e.guest.Calls("online-vcpus"))
}

// A Paused guest cannot answer, and a memory device cannot finish plugging
// while it is paused, so a live resize needs the Machine running.
func TestSetResources_APausedMachineNeedsToBeResumedFirst(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StatePaused)

	requireKind(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), machine.ErrPreconditionFailed)
	assert.Zero(t, e.driver.Calls("resize"))
	assert.Equal(t, small, e.requireState(t, "m", machine.StatePaused).Resources)

	require.NoError(t, e.svc.Resume(ctx, "m"))
	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), "control: Running, the same call resizes")
}

// Removing vCPUs needs no guest action: the VMM removes them and the guest
// reports the new count. The call waits for that count, and does not take a
// count above the target as done.
func TestSetResources_AShrinkWaitsUntilTheGuestReportsTheTargetsVCPUs(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))
	size, err := e.guest.OnlineVcpus(ctx, "m", large.VCPUs)
	require.NoError(t, err)
	require.Equal(t, large.VCPUs, size.VCPUs, "setup: the guest has the grown count online")
	before := e.guest.Calls("online-vcpus")
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.guest.PresentVcpus("m", small.VCPUs)
	}()

	require.NoError(t, e.svc.SetResources(ctx, "m", small, machine.Resources{}))

	assert.Greater(t, e.guest.Calls("online-vcpus")-before, 1, "asked again until the guest reported the smaller count")
	assert.Equal(t, small, e.requireState(t, "m", machine.StateRunning).Resources)
	size, err = e.guest.OnlineVcpus(ctx, "m", small.VCPUs)
	require.NoError(t, err)
	assert.Equal(t, small.VCPUs, size.VCPUs)
}
