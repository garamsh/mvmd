package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// boot_timeout in the configuration is the bound the wired lifecycle puts on
// a guest's boot, so a guest that never finishes leaves a Stopped Machine.
func TestBootTimeout_TheConfiguredBoundReachesTheLifecycle(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	cfg.BootTimeout = "200ms"
	n := start(t, cfg)
	n.app.Substitutes.Images.Provide(digest)
	renew(t, n, 1)
	_, err := n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)

	entered, release := n.app.Substitutes.Guest.Hold("boot")
	defer release()
	done := make(chan error, 1)
	go func() {
		_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "m"})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest was never sent its boot description")
	}
	select {
	case err := <-done:
		requireKind(t, err, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED)
		assert.ErrorContains(t, err, "the guest did not finish booting within 200ms")
	case <-time.After(5 * time.Second):
		t.Fatal("the start never ended")
	}
	got, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, got.GetMachine().GetState())
}
