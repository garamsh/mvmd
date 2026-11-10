package app_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// What the deadline acknowledged is on disk: a daemon started over the same
// state directory finds the renewal, and the Machine records are there.
func TestRestart_AcknowledgedRenewalSurvives(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)

	first := start(t, cfg)
	first.app.Substitutes.Images.Provide(digest)
	_, err := first.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)
	_, err = first.client.RenewDeadline(ctx, &v1alpha2.RenewDeadlineRequest{Length: durationpb.New(time.Hour), RenewalNumber: 9})
	require.NoError(t, err)
	first.shutdown(t)

	second := start(t, cfg)
	resp, err := second.client.InspectDeadline(ctx, &v1alpha2.InspectDeadlineRequest{})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, resp.GetStatus().GetPhase())
	assert.Equal(t, uint64(9), resp.GetStatus().GetLastRenewalNumber())
	assert.Equal(t, time.Hour, resp.GetStatus().GetLength().AsDuration())
	assert.Less(t, resp.GetStatus().GetRemaining().AsDuration(), time.Hour, "the time left, not a fresh hour")
}

func TestMain_Exits(t *testing.T) {
	t.Run("without -config", func(t *testing.T) {
		var out bytes.Buffer
		assert.Equal(t, 2, app.Main(nil, &out))
		assert.Contains(t, out.String(), "-config is required")
	})
	t.Run("with an unknown flag", func(t *testing.T) {
		var out bytes.Buffer
		assert.Equal(t, 2, app.Main([]string{"-nope"}, &out))
	})
	t.Run("with a configuration that is refused", func(t *testing.T) {
		path := filepath.Join(shortDir(t), "bad.json")
		require.NoError(t, os.WriteFile(path, []byte(`{"driver":"memory"}`), 0o600))
		var out bytes.Buffer
		assert.Equal(t, 1, app.Main([]string{"-config", path}, &out))
		assert.Contains(t, out.String(), "mvmd: configuration")
	})
	t.Run("with a configuration that is missing", func(t *testing.T) {
		var out bytes.Buffer
		assert.Equal(t, 1, app.Main([]string{"-config", "/nonexistent/mvmd.json"}, &out))
	})
}
