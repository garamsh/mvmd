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

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/bootclock"
	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	fencingmemory "github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// Substitutes are the in-memory adapters behind the ports when the
// configuration selects the memory driver, so that what a Machine needs on the
// node can be provided: images and disks. Supervisors is nil when the
// configuration names real supervision.
type Substitutes struct {
	Driver      *machinememory.Driver
	Network     *machinememory.Network
	Disks       *machinememory.Disk
	Images      *machinememory.ImageStore
	Supervisors *fencingmemory.Supervisors
	Events      events.Store
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
	clock, err := bootclock.New()
	if err != nil {
		return nil, err
	}
	db, err := boltstore.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}

	// Only the memory driver exists; check() refused any other.
	sub := &Substitutes{
		Driver: machinememory.NewDriver(machine.DriverInfo{
			Name: "memory", Version: "1",
			Capabilities: []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityResize},
		}),
		Network:     machinememory.NewNetwork(),
		Disks:       machinememory.NewDisk(),
		Images:      machinememory.NewImageStore(),
		Supervisors: fencingmemory.NewSupervisors(nil),
	}
	var supervisors fencing.Supervisors = sub.Supervisors
	if s := cfg.Supervision; s != nil {
		dir := s.Dir
		if dir == "" {
			dir = filepath.Join(cfg.StateDir, "machines")
		}
		real, err := supervision.New(supervision.Config{Binary: s.Binary, Dir: dir, Cgroup: s.Cgroup})
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		supervisors, sub.Supervisors = real, nil
	}
	deadline := fencing.NewService(clock, db.Deadline(), supervisors)
	store := events.New(1024)
	sub.Events = store
	machines := machine.NewService(sub.Driver, deadline, sub.Network, sub.Disks, sub.Images, db.Machines(), deadline, cfg.VMMUIDRange.ToUIDRange(), store)

	return &App{
		cfg:         cfg,
		db:          db,
		machines:    machines,
		deadline:    deadline,
		server:      api.New(machines, deadline, []machine.DriverInfo{sub.Driver.Info()}, store),
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
