package api_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func TestGetRuntimeInfo_ReportsEachFigureOfTheNodesCapacity(t *testing.T) {
	// Four different values, so a figure read into the wrong field fails.
	reader := capacitymemory.NewReader(nodecapacity.Capacity{
		CPUs: 6, MemoryBytes: 17 << 30, StateTotalBytes: 500 << 30, StateAvailableBytes: 123 << 30,
	})
	srv := api.New(nil, nil, nil, nil, reader, nil)
	resp, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	c := resp.GetCapacity()
	require.NotNil(t, c, "the capacity is reported")
	assert.EqualValues(t, 6, c.GetCpus())
	assert.EqualValues(t, 17<<30, c.GetMemoryBytes())
	assert.EqualValues(t, 500<<30, c.GetStateTotalBytes())
	assert.EqualValues(t, 123<<30, c.GetStateAvailableBytes())
}

func TestGetRuntimeInfo_ReadsTheCapacityAtEachCall(t *testing.T) {
	reader := capacitymemory.NewReader(nodecapacity.Capacity{StateAvailableBytes: 100})
	srv := api.New(nil, nil, nil, nil, reader, nil)
	first, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.EqualValues(t, 100, first.GetCapacity().GetStateAvailableBytes(), "the setup reported what it was given")

	reader.Set(nodecapacity.Capacity{StateAvailableBytes: 40})
	second, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.EqualValues(t, 40, second.GetCapacity().GetStateAvailableBytes())
}

func TestGetRuntimeInfo_FailsRatherThanReportZeroWhenTheCapacityCannotBeRead(t *testing.T) {
	reader := capacitymemory.NewReader(nodecapacity.Capacity{CPUs: 2})
	srv := api.New(nil, nil, nil, nil, reader, nil)
	_, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err, "control: the same server answers while the capacity reads")

	reader.Fail(errors.New("statfs failed"))
	resp, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	assert.Nil(t, resp)
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, v1alpha2.KindOf(err))
}
