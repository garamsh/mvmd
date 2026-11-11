package held_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity/held"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
)

func TestRead_AddsTheMachineFiguresToWhatTheWrappedReaderMeasures(t *testing.T) {
	measured := capacitymemory.NewReader(nodecapacity.Capacity{CPUs: 4, MemoryBytes: 8 << 30, StateTotalBytes: 100 << 30, StateAvailableBytes: 60 << 30})
	machines := uint32(3)
	r := held.New(measured, 250, func() (uint32, error) { return machines, nil })

	c, err := r.Read()
	require.NoError(t, err)
	assert.Equal(t, nodecapacity.Capacity{
		CPUs: 4, MemoryBytes: 8 << 30, StateTotalBytes: 100 << 30, StateAvailableBytes: 60 << 30,
		MachineLimit: 250, Machines: 3,
	}, c)

	machines = 4
	c, err = r.Read()
	require.NoError(t, err)
	assert.EqualValues(t, 4, c.Machines, "the count is read at each call")
}

func TestRead_FailsWhenTheWrappedReaderOrTheCountDoes(t *testing.T) {
	boom := errors.New("boom")
	measured := capacitymemory.NewReader(nodecapacity.Capacity{CPUs: 1})
	ok := func() (uint32, error) { return 0, nil }

	_, err := held.New(measured, 1, ok).Read()
	require.NoError(t, err, "the control: both read")

	measured.Fail(boom)
	_, err = held.New(measured, 1, ok).Read()
	assert.ErrorIs(t, err, boom)

	measured.Fail(nil)
	_, err = held.New(measured, 1, func() (uint32, error) { return 0, boom }).Read()
	assert.ErrorIs(t, err, boom)
}
