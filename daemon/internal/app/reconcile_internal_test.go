package app

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/bootclock"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

func config(t *testing.T) Config {
	t.Helper()
	dir, err := os.MkdirTemp("", "mvmd")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return Config{
		Socket:   SocketConfig{Path: filepath.Join(dir, "s"), Mode: "0600"},
		StateDir: filepath.Join(dir, "state"),
		Driver:   DriverMemory,
	}
}

// seed writes a deadline record into the state directory, as an earlier run
// of the daemon would have left it.
func seed(t *testing.T, cfg Config, rec func(now fencing.BootTime, bootID string) fencing.Record) {
	t.Helper()
	clock, err := bootclock.New()
	require.NoError(t, err)
	db, err := boltstore.Open(cfg.StateDir)
	require.NoError(t, err)
	require.NoError(t, db.Deadline().Save(context.Background(), rec(clock.Now(), clock.BootID())))
	require.NoError(t, db.Close())
}

// serve starts the daemon and returns a function that stops it.
func serve(t *testing.T, a *App) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Serve(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Error("the daemon did not stop")
		}
	}
}

// A call that arrives before the reconciliation has finished is not served:
// the socket does not exist until the reconciliation is done.
func TestServe_NoCallIsServedBeforeReconciliation(t *testing.T) {
	cfg := config(t)
	a, err := New(cfg)
	require.NoError(t, err)
	reached, release := make(chan struct{}), make(chan struct{})
	a.beforeReconcile = func() { close(reached); <-release }
	stop := serve(t, a)
	defer stop()
	<-reached

	_, statErr := os.Lstat(cfg.Socket.Path)
	require.ErrorIs(t, statErr, os.ErrNotExist, "no socket while the reconciliation has not run")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	_, err = runtimeapi.Dial(ctx, cfg.Socket.Path)
	cancel()
	require.Error(t, err, "a client cannot reach the daemon yet")
	select {
	case <-a.Ready():
		t.Fatal("the daemon was ready before it reconciled")
	default:
	}

	close(release)
	select {
	case <-a.Ready():
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon never became ready")
	}
	conn, err := runtimeapi.Dial(context.Background(), cfg.Socket.Path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
}

// The first call a client makes sees the reconciled deadline, not a blank one.
func TestServe_FirstCallSeesTheReconciledDeadline(t *testing.T) {
	tests := []struct {
		name   string
		record func(now fencing.BootTime, bootID string) fencing.Record
		phase  v1alpha1.DeadlinePhase
		last   uint64
	}{
		{"a live expiry is kept, with the time left", func(now fencing.BootTime, id string) fencing.Record {
			return fencing.Record{Expiry: now.Add(time.Hour), BootID: id, Length: 2 * time.Hour, Renewal: 5}
		}, v1alpha1.DeadlinePhase_DEADLINE_PHASE_ARMED, 5},
		{"a passed expiry stops everything", func(now fencing.BootTime, id string) fencing.Record {
			return fencing.Record{Expiry: now.Add(-time.Second), BootID: id, Length: time.Minute, Renewal: 6}
		}, v1alpha1.DeadlinePhase_DEADLINE_PHASE_STOPPED_EVERYTHING, 6},
		{"another boot is not armed", func(now fencing.BootTime, _ string) fencing.Record {
			return fencing.Record{Expiry: now.Add(time.Hour), BootID: "another-boot", Length: time.Hour, Renewal: 7}
		}, v1alpha1.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := config(t)
			seed(t, cfg, tt.record)
			a, err := New(cfg)
			require.NoError(t, err)
			stop := serve(t, a)
			defer stop()
			<-a.Ready()

			conn, err := runtimeapi.Dial(context.Background(), cfg.Socket.Path)
			require.NoError(t, err)
			defer conn.Close()
			resp, err := v1alpha1.NewRuntimeServiceClient(conn).InspectDeadline(context.Background(), &v1alpha1.InspectDeadlineRequest{})
			require.NoError(t, err)
			assert.Equal(t, tt.phase, resp.GetStatus().GetPhase())
			assert.Equal(t, tt.last, resp.GetStatus().GetLastRenewalNumber())
			if tt.phase == v1alpha1.DeadlinePhase_DEADLINE_PHASE_ARMED {
				assert.Less(t, resp.GetStatus().GetRemaining().AsDuration(), 61*time.Minute, "what is left, not the length")
			}
		})
	}
}

// A reconciliation that fails serves nothing and reports why.
func TestServe_FailedReconciliationServesNothing(t *testing.T) {
	cfg := config(t)
	a, err := New(cfg)
	require.NoError(t, err)
	require.NoError(t, a.db.Close()) // the record cannot be read
	err = a.Serve(context.Background())
	require.ErrorContains(t, err, "reconcile the fencing deadline")
	_, statErr := os.Lstat(cfg.Socket.Path)
	require.ErrorIs(t, statErr, os.ErrNotExist)
}
