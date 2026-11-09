package image_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// pullSteps are the calls of a successful pull in the order it reaches them.
// Each is the point ADR 0007's table has a failure at, with the ports' own
// calls the table folds into a step.
var pullSteps = []string{"locate", "stage", "manifest", "config", "layer", "put-layer", "describe", "start", "boot", "convert", "terminate", "seal", "record", "publish"}

// A failure at any step leaves nothing visible in the cache, and no step
// after it runs. The control of each is the same pull succeeding once the
// failure is withdrawn, which shows the failure was the only thing in its way.
func TestPull_FailureAtAStepLeavesNothingVisible(t *testing.T) {
	// The converter's VMM is ended after a failure from describe to convert,
	// so a terminate that follows them is cleanup and not a later step.
	cleanup := func(failing, later string) bool {
		i := slices.Index(pullSteps, failing)
		return later == "terminate" && i >= slices.Index(pullSteps, "describe") && i <= slices.Index(pullSteps, "convert")
	}

	for i, step := range pullSteps {
		t.Run(step, func(t *testing.T) {
			r := newRig(t)
			ref := r.add("one")
			boom := errors.New("injected at " + step)
			r.failAt(step, boom)

			err := r.pull(ref)

			e := requireKind(t, err, machine.ErrFailed)
			assert.Contains(t, e.Reason, boom.Error())
			assert.NotZero(t, r.calls(step), "the step never ran, so nothing was injected")
			for _, later := range pullSteps[i+1:] {
				if !cleanup(step, later) {
					assert.Zero(t, r.calls(later), "step %s ran after %s failed", later, step)
				}
			}
			assert.Empty(t, r.store.Listed(), "the cache lists an image")
			_, locateErr := r.store.Locate(t.Context(), ref.Digest)
			if step != "locate" { // the injected failure is what Locate answers
				assert.ErrorIs(t, locateErr, machine.ErrImageAbsent)
			}
			assert.Zero(t, r.store.Staging(), "a staging area is left")

			r.heal(step)
			require.NoError(t, r.pull(ref), "the pull did not succeed once the failure was withdrawn")
			r.requireListed(t, ref)
			assert.Zero(t, r.store.Staging())
			_, running := r.sup.Running(conversionID(ref))
			assert.False(t, running)
		})
	}
}

// A failure of the converter's VMM, wherever it came from, leaves the VMM
// ended and its user released; one that cannot be ended leaves both, because a
// VMM may still run as that user.
func TestPull_ConverterVMEndsWhateverHappened(t *testing.T) {
	for _, step := range []string{"describe", "start", "boot", "convert", "seal", "record", "publish"} {
		t.Run("a failed "+step+" ends it", func(t *testing.T) {
			r := newRig(t)
			ref := r.add("one")
			r.failAt(step, errors.New("injected"))

			require.Error(t, r.pull(ref))

			r.requireNothingVisible(t, ref)
			assert.Equal(t, 1, r.calls("terminate"), "the converter's VMM was not ended")
		})
	}

	t.Run("a VMM that cannot be ended keeps its user and fails the pull before the output is used", func(t *testing.T) {
		r := newRig(t)
		ref := r.add("one")
		r.failAt("terminate", errors.New("injected"))

		err := requireKind(t, r.pull(ref), machine.ErrFailed)

		assert.Contains(t, err.Reason, "end the converter's VMM")
		assert.Zero(t, r.calls("seal"), "the output was used while the VMM may still run")
		assert.Equal(t, 1, r.uids.Held(), "the user was released with a VMM that may still run")
		assert.Empty(t, r.store.Listed())
		assert.Zero(t, r.store.Staging())

		// The control: where the VMM ends, the user is released.
		r.heal("terminate")
		require.NoError(t, r.pull(ref))
		assert.Equal(t, 1, r.uids.Held(), "only the first attempt's user is held")
	})
}

// A publish that fails has moved its blobs, which no listed image reaches
// until the sweep removes them (ADR 0007 step 9).
func TestPull_FailedPublishLeavesUnreferencedBlobsTheSweepRemoves(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("publish", errors.New("injected"))

	require.Error(t, r.pull(ref))

	r.requireNothingVisible(t, ref)
	assert.NotZero(t, r.store.Unreferenced(), "the blobs were not moved before the replace, so the case is not the one under test")

	require.NoError(t, r.svc.Sweep(t.Context()))
	assert.Zero(t, r.store.Unreferenced())
}

// A staging area that cannot be removed after a failure is reported with it.
func TestPull_FailureAndFailedCleanupAreBothReported(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("convert", errors.New("converter broke"))
	r.failAt("discard", errors.New("disk gone"))

	err := requireKind(t, r.pull(ref), machine.ErrFailed)

	assert.Contains(t, err.Reason, "converter broke")
	assert.Contains(t, err.Reason, "disk gone")
	assert.Empty(t, r.store.Listed())
	assert.Equal(t, 1, r.store.Staging(), "the staging area is what the sweep removes")

	require.NoError(t, r.svc.Sweep(t.Context()))
	assert.Zero(t, r.store.Staging())
}

// Once the image is visible a failed removal of staging changes nothing a
// caller sees (ADR 0007 step 10).
func TestPull_FailedRemovalAfterThePublishIsSuccess(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	r.failAt("discard", errors.New("injected"))

	require.NoError(t, r.pull(ref))

	r.requireListed(t, ref)
	assert.Equal(t, 1, r.store.Staging())

	// The control: the sweep removes the leftover and keeps the image.
	require.NoError(t, r.svc.Sweep(t.Context()))
	assert.Zero(t, r.store.Staging())
	r.requireListed(t, ref)
}
