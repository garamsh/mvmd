package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func execOnA(request string) *v1alpha2.ExecRequest {
	return &v1alpha2.ExecRequest{ExecId: "x", RequestId: request, MachineId: "a", Command: []string{"echo", "hi"}}
}

// The exec records are in the state directory: a restart keeps a request ID's
// conflict whether the Machine's VMM lives or ended, and a record of a Machine
// whose VMM ended is the cut-short one.
func TestRestore_KeepsExecRecords(t *testing.T) {
	ctx := context.Background()

	t.Run("supervisor alive", func(t *testing.T) {
		cfg := testConfig(t)
		first := start(t, cfg)
		populate(t, first)
		_, err := first.client.Exec(ctx, execOnA("r"))
		require.NoError(t, err)
		first.shutdown(t)

		second := startWith(t, cfg, superviseA)
		_, err = second.client.Exec(ctx, execOnA("other"))
		requireKind(t, err, conflict)
		resp, err := second.client.Exec(ctx, execOnA("r"))
		require.NoError(t, err, "control: the original request ID")
		assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_STARTED, resp.GetRecord().GetStatus())
	})
	t.Run("no supervisor", func(t *testing.T) {
		cfg := testConfig(t)
		first := start(t, cfg)
		populate(t, first)
		_, err := first.client.Exec(ctx, execOnA("r"))
		require.NoError(t, err)
		first.shutdown(t)

		second := startWith(t, cfg, provide)
		_, err = second.client.Exec(ctx, execOnA("other"))
		requireKind(t, err, conflict)
		resp, err := second.client.Exec(ctx, execOnA("r"))
		require.NoError(t, err)
		assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_CUT_SHORT, resp.GetRecord().GetStatus())
		second.shutdown(t)

		third := startWith(t, cfg, provide)
		resp, err = third.client.Exec(ctx, execOnA("r"))
		require.NoError(t, err)
		assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_CUT_SHORT, resp.GetRecord().GetStatus(), "kept by the next restart too")
	})
}
