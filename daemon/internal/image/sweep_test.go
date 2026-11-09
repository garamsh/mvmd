package image_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The sweep removes what a crashed pull left, and nothing a listed image
// reaches.
func TestSweep_RemovesWhatACrashedPullLeftAndKeepsListedImages(t *testing.T) {
	r := newRig(t)
	kept := r.add("kept")
	require.NoError(t, r.pull(kept))
	crashed := r.add("crashed")
	r.store.Leave(crashed.Digest)
	require.Equal(t, 1, r.store.Staging(), "the setup did not leave a staging area")
	require.Equal(t, 1, r.store.Unreferenced(), "the setup did not leave a blob")

	require.NoError(t, r.svc.Sweep(t.Context()))

	assert.Zero(t, r.store.Staging())
	assert.Zero(t, r.store.Unreferenced())
	r.requireListed(t, kept)
	_, err := r.store.Locate(t.Context(), crashed.Digest)
	assert.ErrorIs(t, err, machine.ErrImageAbsent)
}

func TestSweep_FailureIsReported(t *testing.T) {
	r := newRig(t)
	r.store.Leave(r.add("crashed").Digest)
	r.failAt("sweep", errors.New("injected"))

	err := requireKind(t, r.svc.Sweep(t.Context()), machine.ErrFailed)

	assert.Contains(t, err.Reason, "injected")
	assert.Equal(t, 1, r.store.Staging(), "the leftover was removed by a sweep that failed")

	// The control: the same leftover goes once the sweep works.
	r.heal("sweep")
	require.NoError(t, r.svc.Sweep(t.Context()))
	assert.Zero(t, r.store.Staging())
}

// The sweep is its own operation: a pull does not run it.
func TestPull_DoesNotSweep(t *testing.T) {
	r := newRig(t)
	require.NoError(t, r.pull(r.add("one")))
	assert.Zero(t, r.calls("sweep"))

	require.NoError(t, r.svc.Sweep(t.Context()))
	assert.Equal(t, 1, r.calls("sweep"))
}
