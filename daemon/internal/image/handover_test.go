package image_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// firstStaging is the staging area of the first pull of a rig, which the
// memory Store names as it opens it.
var firstStaging = image.Staging{ID: "staging-1"}

// The converter's user holds the output while the converter runs, and no
// longer holds it once the pull has sealed it.
func TestPull_ConverterUserHoldsTheOutputWhileItConverts(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	var during struct {
		lent bool
		user machine.User
	}
	r.conv.Serve(func(_ context.Context, _ machine.MachineID, _ image.ConversionJob) (uint64, error) {
		during.user, during.lent = r.store.Lent(firstStaging)
		return memory.DefaultLength, nil
	})

	require.NoError(t, r.pull(ref))

	require.True(t, during.lent, "the output was not the converter's user's while it converted")
	assert.Equal(t, machine.User{UID: firstUID, GID: firstUID}, during.user)
	assert.Equal(t, 1, r.calls("lend"))
	assert.Equal(t, 1, r.calls("reclaim"))
	r.requireListed(t, ref)
}

// The output goes back to the daemon after the VMM has ended and before it is
// sealed, on success and on each failure of the conversion, a VMM that never
// boots among them.
func TestPull_OutputIsTakenBackOnceTheVMMHasEnded(t *testing.T) {
	cases := []struct {
		name string
		step string
	}{
		{"success", ""},
		{"the VMM is not described", "describe"},
		{"the VMM does not start", "start"},
		{"the VMM never boots", "boot"},
		{"the converter fails", "convert"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := newRig(t)
			ref := r.add("one")
			var vmmRunning, reclaimed bool
			r.store.OnReclaim(func(image.Staging) {
				_, vmmRunning = r.sup.Running(conversionID(ref))
				reclaimed = true
			})
			if c.step != "" {
				r.failAt(c.step, errors.New("broken"))
				// The staging area survives the failed pull, so what it
				// holds can be read.
				r.failAt("discard", errors.New("kept"))
			}

			err := r.pull(ref)

			require.True(t, reclaimed, "the output was not taken back")
			assert.False(t, vmmRunning, "the output was taken back while the VMM ran")
			assert.Zero(t, r.uids.Held(), "the converter's user is held")
			if c.step == "" {
				require.NoError(t, err)
				return
			}
			requireKind(t, err, machine.ErrFailed)
			_, lent := r.store.Lent(firstStaging)
			assert.False(t, lent, "the converter's user holds the output after the pull failed")
			assert.Zero(t, r.calls("seal"))
		})
	}
}

// A VMM that cannot be ended may still hold the output open, so it is not
// taken back, and the user stays held, as the VMM does.
func TestPull_OutputIsNotTakenBackFromAVMMThatCannotBeEnded(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("terminate", errors.New("stuck"))
	r.failAt("discard", errors.New("kept"))

	err := r.pull(ref)

	requireKind(t, err, machine.ErrFailed)
	assert.Zero(t, r.calls("reclaim"))
	assert.Zero(t, r.calls("seal"))
	assert.Equal(t, 1, r.uids.Held())
	_, lent := r.store.Lent(firstStaging)
	assert.True(t, lent, "the output was taken back from a VMM that may be running")
}

// An output that cannot be taken back is not sealed, and keeps the user held,
// since a reused user could change what the daemon is checking.
func TestPull_OutputThatCannotBeTakenBackFailsThePull(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("reclaim", errors.New("no chown"))

	err := r.pull(ref)

	e := requireKind(t, err, machine.ErrFailed)
	assert.Contains(t, e.Reason, "take the output back")
	assert.Zero(t, r.calls("seal"))
	assert.Equal(t, 1, r.uids.Held())
	assert.Empty(t, r.store.Listed())

	// The control: with the output taken back, the same pull succeeds.
	r.heal("reclaim")
	require.NoError(t, r.pull(ref))
	r.requireListed(t, ref)
}

// A pull whose handover fails does not boot the converter, and frees the user.
func TestPull_FailedHandoverDoesNotBootTheConverter(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("lend", errors.New("no chown"))

	err := r.pull(ref)

	e := requireKind(t, err, machine.ErrFailed)
	assert.Contains(t, e.Reason, "give the output to the converter's user")
	assert.Zero(t, r.calls("describe"))
	assert.Zero(t, r.calls("start"))
	assert.Zero(t, r.calls("boot"))
	assert.Zero(t, r.calls("convert"))
	r.requireNothingVisible(t, ref)

	// The control: with the handover working, the same pull boots and lists.
	r.heal("lend")
	require.NoError(t, r.pull(ref))
	assert.Equal(t, 1, r.calls("boot"))
	r.requireListed(t, ref)
}
