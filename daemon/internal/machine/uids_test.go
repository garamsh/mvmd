package machine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A Machine and a conversion drawing from one range never get the same UID,
// even where the identifier of one spells the identifier of the other.
func TestUIDs_AMachineAndAConversionNeverShareAUID(t *testing.T) {
	uids := machine.NewUIDs(machine.UIDRange{First: 500, Count: 3})

	m, err := uids.AllocateMachine("sha256:aa")
	require.NoError(t, err)
	c, err := uids.AllocateConversion("sha256:aa")
	require.NoError(t, err)

	assert.NotEqual(t, m, c.UID)
	assert.Equal(t, c.UID, c.GID)
	assert.Equal(t, 2, uids.Held())

	// Releasing one leaves the other held and its UID out of reach.
	uids.ReleaseMachine("sha256:aa")
	assert.Equal(t, 1, uids.Held())
	again, err := uids.AllocateMachine("other")
	require.NoError(t, err)
	assert.Equal(t, m, again, "the released UID is the lowest free one")
	assert.NotEqual(t, c.UID, again)
}

// A range full of Machines and conversions refuses the next holder of either
// kind; the control is that releasing one of them frees exactly one UID.
func TestUIDs_ExhaustedRangeRefusesBothKinds(t *testing.T) {
	uids := machine.NewUIDs(machine.UIDRange{First: 500, Count: 2})
	_, err := uids.AllocateMachine("m")
	require.NoError(t, err)
	_, err = uids.AllocateConversion("sha256:aa")
	require.NoError(t, err)

	_, err = uids.AllocateMachine("n")
	assert.ErrorIs(t, err, machine.ErrPreconditionFailed)
	_, err = uids.AllocateConversion("sha256:bb")
	assert.ErrorIs(t, err, machine.ErrPreconditionFailed)

	uids.ReleaseConversion("sha256:aa")
	_, err = uids.AllocateMachine("n")
	assert.NoError(t, err, "the control: a released UID is allocatable")
	_, err = uids.AllocateConversion("sha256:bb")
	assert.ErrorIs(t, err, machine.ErrPreconditionFailed)
}
