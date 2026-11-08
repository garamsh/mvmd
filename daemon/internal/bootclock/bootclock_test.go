package bootclock_test

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/bootclock"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

var _ fencing.Clock = (*bootclock.Clock)(nil)

func TestClock_BootIDIsTheHostsAndStable(t *testing.T) {
	a, err := bootclock.New()
	require.NoError(t, err)
	b, err := bootclock.New()
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f-]{36}$`), a.BootID())
	assert.Equal(t, a.BootID(), b.BootID())
}

func TestClock_NowAdvances(t *testing.T) {
	c, err := bootclock.New()
	require.NoError(t, err)
	first := c.Now()
	assert.Positive(t, first)
	time.Sleep(5 * time.Millisecond)
	assert.GreaterOrEqual(t, c.Now().Sub(first), 5*time.Millisecond)
}

func TestClock_WaitReturnsWhenBootTimeReachesTheTarget(t *testing.T) {
	c, err := bootclock.New()
	require.NoError(t, err)
	until := c.Now().Add(30 * time.Millisecond)
	require.NoError(t, c.Wait(context.Background(), until))
	assert.GreaterOrEqual(t, c.Now(), until, "it did not return early")
	require.NoError(t, c.Wait(context.Background(), c.Now().Add(-time.Hour)), "a past target returns at once")
}

func TestClock_WaitEndsWithItsContext(t *testing.T) {
	c, err := bootclock.New()
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, c.Wait(ctx, c.Now().Add(time.Hour)), context.DeadlineExceeded)
}
