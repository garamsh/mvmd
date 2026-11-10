package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// createHeldPastTheBound creates "m" with its root disk held for 150ms, and
// returns the Create's outcome.
func createHeldPastTheBound(t *testing.T, callTimeout string) (*node, error) {
	t.Helper()
	cfg := testConfig(t)
	cfg.CallTimeout = callTimeout
	n := start(t, cfg)
	n.app.Substitutes.Images.Provide(digest)
	renew(t, n, 1)
	entered, release := n.app.Substitutes.Disks.Hold("create-root")
	done := make(chan error, 1)
	go func() {
		_, err := n.client.CreateMachine(context.Background(), create("m"))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		release()
		t.Fatal("the root disk was never asked for")
	}
	time.Sleep(150 * time.Millisecond)
	release()
	select {
	case err := <-done:
		return n, err
	case <-time.After(5 * time.Second):
		t.Fatal("the create never ended")
		return n, nil
	}
}

// call_timeout in the configuration is the bound the wired lifecycle puts on an
// accepted call: a Create held past it fails and leaves no Machine, while the
// default bound lets the same Create finish.
func TestCallTimeout_TheConfiguredBoundReachesTheLifecycle(t *testing.T) {
	n, err := createHeldPastTheBound(t, "50ms")
	requireKind(t, err, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED)
	_, err = n.client.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: "m"})
	requireKind(t, err, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND)

	// Control: the default bound.
	n, err = createHeldPastTheBound(t, "")
	require.NoError(t, err)
	got, err := n.client.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_CREATED, got.GetMachine().GetState())
}
