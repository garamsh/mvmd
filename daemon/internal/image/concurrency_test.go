package image_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Pulls of one digest are one pull: one staging area, one conversion, and every
// caller gets the outcome. Callers that join do so while the first pull is held
// in the converter, where the image cannot yet be listed, so none can have
// found it present.
func TestPull_ConcurrentPullsOfOneDigestAreOnePull(t *testing.T) {
	const callers = 8
	r := newRig(t)
	ref := r.add("one")
	entered, release := r.conv.Hold("convert")

	errs := make(chan error, callers)
	go func() { errs <- r.pull(ref) }()
	<-entered // the first caller's pull is in the converter
	for range callers - 1 {
		go func() { errs <- r.pull(ref) }()
	}
	r.awaitJoined(t, callers-1)
	release()

	for range callers {
		require.NoError(t, <-errs)
	}
	assert.Equal(t, 1, r.calls("stage"), "a second pull of the digest began")
	assert.Equal(t, 1, r.calls("convert"))
	assert.Equal(t, 1, r.calls("publish"))
	assert.Equal(t, 1, r.calls("terminate"))
	r.requireListed(t, ref)
}

// The control: pulls of different digests are not one pull.
func TestPull_ConcurrentPullsOfDifferentDigestsAreSeparatePulls(t *testing.T) {
	const callers = 4
	r := newRig(t)
	var wg sync.WaitGroup
	for i := range callers {
		ref := r.add(string(rune('a' + i)))
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, r.pull(ref))
		}()
	}
	wg.Wait()

	assert.Equal(t, callers, r.calls("convert"))
	assert.Len(t, r.store.Listed(), callers)
}

// A failed shared pull fails every caller waiting on it, and a later call
// starts a pull of its own.
func TestPull_WaitingCallersShareAFailureAndALaterCallRetries(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	entered, release := r.conv.Hold("convert")
	r.failAt("convert", assert.AnError)

	errs := make(chan error, 2)
	go func() { errs <- r.pull(ref) }()
	<-entered
	go func() { errs <- r.pull(ref) }()
	r.awaitJoined(t, 1)
	release()

	require.Error(t, <-errs)
	require.Error(t, <-errs)
	assert.Equal(t, 1, r.calls("stage"), "the second caller started a pull instead of waiting for the first")

	r.heal("convert")
	require.NoError(t, r.pull(ref))
	assert.Equal(t, 2, r.calls("stage"))
}

// A caller that gives up does not stop the pull, whether it started it or
// joined it: the pull runs to its end, and the other caller has its outcome.
func TestPull_CallerLeavingDoesNotStopThePull(t *testing.T) {
	r := newRig(t)
	ref := r.add("one")
	entered, release := r.conv.Hold("convert")

	starter, leave := context.WithCancel(context.Background())
	left := make(chan error, 1)
	go func() { left <- r.svc.Pull(starter, ref) }()
	<-entered
	stayed := make(chan error, 1)
	go func() { stayed <- r.pull(ref) }()
	r.awaitJoined(t, 1)

	leave()
	require.ErrorIs(t, <-left, context.Canceled)
	release()

	require.NoError(t, <-stayed)
	r.requireListed(t, ref)
	assert.Equal(t, 1, r.calls("convert"))
}

// Commits are applied one at a time (ADR 0007): while one image's publish is
// running another's waits, though their pulls are otherwise independent.
func TestPull_PublishesOfDifferentImagesAreSerialized(t *testing.T) {
	r := newRig(t)
	a, b := r.add("a"), r.add("b")
	entered, release := r.store.Hold("publish")

	errs := make(chan error, 2)
	go func() { errs <- r.pull(a) }()
	<-entered // a's publish is running
	go func() { errs <- r.pull(b) }()
	require.Eventually(t, func() bool { return r.calls("record") == 2 }, 5*time.Second, time.Millisecond, "the second pull did not reach its publish")
	release()
	require.NoError(t, <-errs)
	require.NoError(t, <-errs)

	assert.False(t, r.store.PublishOverlapped(), "two publishes ran at once")
	assert.Len(t, r.store.Listed(), 2)
}
