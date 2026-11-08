package supervision_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// daemon is the wired app serving over a state directory, with real
// supervision when the config names it.
type daemon struct {
	app    *app.App
	client v1alpha1.RuntimeServiceClient
	stop   func()
}

func startDaemon(t *testing.T, cfg app.Config, work string) *daemon {
	t.Helper()
	a, err := app.New(cfg)
	require.NoError(t, err)
	a.Substitutes.Images.Provide(digest)
	a.Substitutes.Driver.UseProcess(vmmProcess(work))
	done := make(chan error, 1)
	cctx, cancel := context.WithCancel(ctx)
	go func() { done <- a.Serve(cctx) }()
	select {
	case <-a.Ready():
	case err := <-done:
		t.Fatalf("the daemon ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon was not ready")
	}
	conn, err := runtimeapi.Dial(ctx, cfg.Socket.Path)
	require.NoError(t, err)
	d := &daemon{app: a, client: v1alpha1.NewRuntimeServiceClient(conn)}
	d.stop = func() {
		_ = conn.Close()
		cancel()
		require.NoError(t, <-done)
	}
	t.Cleanup(func() {
		if d.stop != nil {
			d.stop()
		}
	})
	return d
}

func (d *daemon) shutdown() {
	d.stop()
	d.stop = nil
}

// wiredConfig returns the config of a daemon over a fresh state directory and
// the run directory its Machine "m" gets from the supervision adapter.
func wiredConfig(t *testing.T) (cfg app.Config, work string) {
	dir := shortDir(t)
	work = filepath.Join(dir, "state", "machines", "m", "run")
	// The config names no cgroup for the VMMs, so the adapter's default is the
	// parent this test leaves behind.
	t.Cleanup(func() { _ = os.Remove(supervision.DefaultVMMCgroup) })
	return app.Config{
		Socket:      app.SocketConfig{Path: filepath.Join(dir, "s"), Mode: "0600"},
		StateDir:    filepath.Join(dir, "state"),
		Driver:      app.DriverMemory,
		VMMUIDRange: app.VMMUIDRangeConfig{First: 65534, Count: 1},
		Supervision: &app.SupervisionConfig{Binary: supervisorBin},
	}, work
}

func createStart(t *testing.T, d *daemon) {
	t.Helper()
	_, err := d.client.CreateMachine(ctx, &v1alpha1.CreateMachineRequest{
		MachineId: "m", RequestId: "r", ImageDigest: digest,
		Resources:     &v1alpha1.Resources{CpuCount: 1, MemoryBytes: 1 << 30},
		RootDiskBytes: 1 << 30,
		NetworkPolicy: &v1alpha1.NetworkPolicy{},
	})
	require.NoError(t, err)
	_, err = d.client.RenewDeadline(ctx, &v1alpha1.RenewDeadlineRequest{Length: durationpb.New(time.Hour), RenewalNumber: 1})
	require.NoError(t, err)
	_, err = d.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
}

// A config that names supervision runs the Machine under a real supervisor, and
// the substitute is not in the graph.
func TestWired_ARealSupervisorRunsTheMachine(t *testing.T) {
	cfg, work := wiredConfig(t)
	d := startDaemon(t, cfg, work)
	assert.Nil(t, d.app.Substitutes.Supervisors, "the in-memory substitute is not wired")
	createStart(t, d)
	untilReady(t, work)
	ns := namespaceOf(supervisorOf(t, "m"))
	requireTree(t, ns)

	// The Driver was given the run directory the supervisor started in.
	assert.Equal(t, work, d.app.Substitutes.Driver.BootSpec("m").RunDir)

	// The guest's memory comes from the Machine through the lifecycle, and the
	// overhead from the Driver's description.
	limit, err := os.ReadFile(filepath.Join(supervision.DefaultVMMCgroup, "m", "memory.max"))
	require.NoError(t, err)
	assert.Equal(t, strconv.Itoa(1<<30+256<<20), strings.TrimSpace(string(limit)))

	_, err = d.client.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(time.Second)})
	require.NoError(t, err)
	assert.Empty(t, members(ns), "stopping the Machine ends its supervisor and the namespace")
	assert.Empty(t, processes("--machine m "))
}

// A daemon that restarts finds the supervisor of a Machine it recorded as
// Running, keeps the Machine Running, and still stops it.
func TestWired_ARestartedDaemonReattaches(t *testing.T) {
	cfg, work := wiredConfig(t)
	first := startDaemon(t, cfg, work)
	createStart(t, first)
	untilReady(t, work)
	ns := namespaceOf(supervisorOf(t, "m"))
	first.shutdown()
	assert.NotEmpty(t, members(ns), "the supervisor outlives the daemon")

	second := startDaemon(t, cfg, work)
	got, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, got.GetMachine().GetState())
	_, err = second.client.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(time.Second)})
	require.NoError(t, err)
	assert.Empty(t, members(ns))

	// Control: with the supervisor gone, a third start finds the Machine Stopped.
	second.shutdown()
	third := startDaemon(t, cfg, work)
	got, err = third.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, got.GetMachine().GetState())
}
