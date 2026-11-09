// Package app is mvmd's composition site: the one place that constructs the
// adapters, wires them to the inside and serves the runtime API. cmd/mvmd
// calls Main and holds nothing else, so a test reaches the wired graph the
// binary ships (stack-go.md §1 "Project envelope").
package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/bootclock"
	"github.com/garamsh/mvmd/daemon/internal/channel"
	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	fencingmemory "github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// eventsKept is how many events the node holds for a client that resumes.
// runtime-api.md leaves how long events are kept open.
const eventsKept = 1024

// wallClock is the host's wall clock, which a restored guest is set to.
type wallClock struct{}

func (wallClock) Now() time.Time { return time.Now() }

// Substitutes are the in-memory adapters behind the ports when the
// configuration selects the memory driver, so that what a Machine needs on the
// node can be provided: images and disks. Supervisors is nil when the
// configuration names real supervision, and Driver and Guest are nil when it
// names the Firecracker Driver.
type Substitutes struct {
	Driver      *machinememory.Driver
	Network     *machinememory.Network
	Disks       *machinememory.Disk
	Images      *machinememory.ImageStore
	Snapshots   *machinememory.SnapshotStore
	Guest       *machinememory.Guest
	Supervisors *fencingmemory.Supervisors
}

// App is the wired daemon.
type App struct {
	cfg         Config
	db          *boltstore.DB
	machines    machine.Service
	deadline    fencing.Service
	server      *api.Server
	Substitutes *Substitutes

	ready chan struct{}
	// beforeReconcile, when set, runs before the reconciliation. Tests use it
	// to hold the daemon there.
	beforeReconcile func()
}

// New builds the graph: it opens the database and constructs every adapter the
// configuration selects. Nothing is served until Serve.
func New(cfg Config) (*App, error) {
	if err := cfg.check(); err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	bootTimeout, err := cfg.bootTimeout()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	callTimeout, err := cfg.callTimeout()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	clock, err := bootclock.New()
	if err != nil {
		return nil, err
	}
	db, err := boltstore.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}

	sub := &Substitutes{
		Driver: machinememory.NewDriver(machine.DriverInfo{
			Name: "memory", Version: "1", CPUModel: "memory-cpu",
			Capabilities: []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityResize, machine.CapabilitySnapshot},
		}),
		Network:     machinememory.NewNetwork(),
		Disks:       machinememory.NewDisk(),
		Images:      machinememory.NewImageStore(),
		Snapshots:   machinememory.NewSnapshotStore(),
		Guest:       machinememory.NewGuest(0),
		Supervisors: fencingmemory.NewSupervisors(nil),
	}
	var network machine.Network = sub.Network
	if n := cfg.Network; n != nil {
		hc, err := n.toHostnet(cfg.VMMUIDRange)
		if err == nil {
			network, err = hostnet.New(hc)
		}
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		sub.Network = nil
	}
	var supervisors fencing.Supervisors = sub.Supervisors
	if s := cfg.Supervision; s != nil {
		dir := s.Dir
		if dir == "" {
			dir = filepath.Join(cfg.StateDir, "machines")
		}
		real, err := supervision.New(supervision.Config{Binary: s.Binary, Dir: dir, Cgroup: s.Cgroup, VMMCgroup: s.VMMCgroup})
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		supervisors, sub.Supervisors = real, nil
	}
	// The Firecracker Driver replaces the in-memory Driver, and the Guest Agent
	// channel adapter over it replaces the in-memory Guest. Its Machines'
	// run directories are the supervisors' (driver.md).
	var driver machine.Driver = sub.Driver
	var guest machine.Guest = sub.Guest
	if f := cfg.Firecracker; f != nil {
		d, err := firecracker.New(f.toDriver(supervisors))
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		driver, guest = d, channel.NewGuest(d)
		sub.Driver, sub.Guest = nil, nil
	}
	log := events.NewService(eventsKept)
	deadline := fencing.NewService(clock, db.Deadline(), supervisors, log)
	machines := machine.NewService(driver, deadline, network, sub.Disks, sub.Images, sub.Snapshots, guest, wallClock{}, db.Machines(), deadline, machine.NewUIDs(cfg.VMMUIDRange.ToUIDRange()), log, machine.WithBootTimeout(bootTimeout), machine.WithCallTimeout(callTimeout))

	return &App{
		cfg:         cfg,
		db:          db,
		machines:    machines,
		deadline:    deadline,
		server:      api.New(machines, deadline, []machine.DriverInfo{driver.Info()}, log),
		Substitutes: sub,
		ready:       make(chan struct{}),
	}, nil
}

// Ready is closed when the socket is listening, which is after the
// reconciliation.
func (a *App) Ready() <-chan struct{} { return a.ready }

// Serve restores the Machines from the state directory and reconciles the
// fencing deadline against them, then serves the runtime API on the
// socket until the context ends or the deadline's expiry loop fails. No call
// is served before the reconciliation has finished: the socket does not exist
// until then. It closes the database before it returns.
func (a *App) Serve(ctx context.Context) error {
	defer a.db.Close()
	if a.beforeReconcile != nil {
		a.beforeReconcile()
	}
	if err := a.machines.Restore(ctx); err != nil {
		return fmt.Errorf("restore the Machines: %w", err)
	}
	if err := a.deadline.Reconcile(ctx, a.machines); err != nil {
		return fmt.Errorf("reconcile the fencing deadline: %w", err)
	}
	l, err := listen(a.cfg.Socket)
	if err != nil {
		return err
	}

	g := grpc.NewServer(
		grpc.UnaryInterceptor(api.Interceptor),
		grpc.StreamInterceptor(api.StreamInterceptor),
	)
	a.server.Register(g)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := a.deadline.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			errs <- fmt.Errorf("fencing deadline: %w", err)
		}
		cancel()
	}()
	go func() {
		defer wg.Done()
		<-ctx.Done()
		a.server.Stop()
		g.GracefulStop()
	}()
	close(a.ready)
	serveErr := g.Serve(l)
	cancel()
	wg.Wait()
	close(errs)
	return errors.Join(append([]error{serveErr}, drain(errs)...)...)
}

func drain(c <-chan error) []error {
	var out []error
	for err := range c {
		out = append(out, err)
	}
	return out
}

// Main is the binary: it reads the configuration named by -config, serves
// until interrupted, and returns the process's exit code. It logs once, here.
func Main(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("mvmd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", "", "path of the configuration file")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *path == "" {
		fmt.Fprintln(stderr, "mvmd: -config is required")
		return 2
	}
	cfg, err := LoadConfig(*path)
	if err != nil {
		fmt.Fprintln(stderr, "mvmd:", err)
		return 1
	}
	a, err := New(cfg)
	if err != nil {
		fmt.Fprintln(stderr, "mvmd:", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := a.Serve(ctx); err != nil {
		fmt.Fprintln(stderr, "mvmd:", err)
		return 1
	}
	return 0
}
