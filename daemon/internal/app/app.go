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
	"net/netip"
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
	"github.com/garamsh/mvmd/daemon/internal/bootsetstore"
	"github.com/garamsh/mvmd/daemon/internal/events"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	fencingmemory "github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/hostcapacity"
	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	imagememory "github.com/garamsh/mvmd/daemon/internal/image/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity/held"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
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
// names a real Driver. Disks, Snapshots, Images, the Registry, the
// Converter, the Cache and the conversion Driver and Supervision are nil under
// it too: they are the real adapters then. Images is what a Machine's start
// locates an image in and Cache is where a pull publishes, so under the memory
// driver a pulled image is not one a Machine can boot; a test provides the one
// it boots.
type Substitutes struct {
	Driver                *machinememory.Driver
	Network               *machinememory.Network
	Disks                 *machinememory.Disk
	Images                *machinememory.ImageStore
	Snapshots             *machinememory.SnapshotStore
	DiskSnapshots         *machinememory.DiskSnapshotStore
	Guest                 *machinememory.Guest
	Supervisors           *fencingmemory.Supervisors
	Registry              *imagememory.Registry
	Converter             *imagememory.Converter
	Cache                 *imagememory.Store
	ConversionDriver      *machinememory.Driver
	ConversionSupervision *machinememory.Supervision
	Capacity              *capacitymemory.Reader
}

// App is the wired daemon.
type App struct {
	cfg         Config
	db          *boltstore.DB
	machines    machine.Service
	deadline    fencing.Service
	images      images
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
	return build(cfg, func() (fencing.Clock, error) { return bootclock.New() })
}

// build is New with the clock the deadline and the boot identity read named by
// newClock, so a test outside the package can end a boot between two daemons.
func build(cfg Config, newClock func() (fencing.Clock, error)) (*App, error) {
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
	pullTimeout, err := cfg.pullTimeout()
	if err != nil {
		return nil, fmt.Errorf("configuration: %w", err)
	}
	clock, err := newClock()
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
			Capabilities: []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityCPUResize, machine.CapabilityMemoryResize, machine.CapabilitySnapshot},
		}),
		Network:       machinememory.NewNetwork(),
		Disks:         machinememory.NewDisk(),
		Images:        machinememory.NewImageStore(),
		Snapshots:     machinememory.NewSnapshotStore(),
		DiskSnapshots: machinememory.NewDiskSnapshotStore(),
		Guest:         machinememory.NewGuest(0),
		Supervisors:   fencingmemory.NewSupervisors(nil),
		Capacity:      capacitymemory.NewReader(nodecapacity.Capacity{CPUs: 4, MemoryBytes: 8<<30 + 1, StateTotalBytes: 100<<30 + 1, StateAvailableBytes: 60<<30 + 1}),
	}
	var network machine.Network = sub.Network
	machineLimit := cfg.VMMUIDRange.Count
	var prefixes []netip.Prefix
	if n := cfg.Network; n != nil {
		hc, err := n.toHostnet(cfg.VMMUIDRange)
		if err == nil {
			err = hc.CheckForwarding(os.ReadFile)
		}
		if err == nil {
			network, err = hostnet.New(hc)
			machineLimit = min(machineLimit, hc.MachineAddresses())
			for _, p := range []netip.Prefix{hc.IPv4Prefix, hc.IPv6Prefix} {
				if p.IsValid() {
					prefixes = append(prefixes, p)
				}
			}
		}
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		sub.Network = nil
	}
	var supervisors fencing.Supervisors = sub.Supervisors
	var runDirs string
	if s := cfg.Supervision; s != nil {
		dir := s.Dir
		if dir == "" {
			dir = filepath.Join(cfg.StateDir, "machines")
		}
		runDirs = dir
		real, err := supervision.New(supervision.Config{Binary: s.Binary, Dir: dir, Cgroup: s.Cgroup, VMMCgroup: s.VMMCgroup})
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		supervisors, sub.Supervisors = real, nil
	}
	// The selected real Driver replaces the in-memory Driver, and the Guest
	// Agent channel adapter over it replaces the in-memory Guest. Its Machines'
	// run directories are the supervisors' (driver.md).
	var driver machine.Driver = sub.Driver
	var guest machine.Guest = sub.Guest
	var capacity nodecapacity.Reader = sub.Capacity
	kernels := machinememory.Kernels()
	bootSets := machinememory.BootSets()
	var bootFiles machine.BootFileChecker = &machinememory.BootFiles{}
	var addedSets machine.BootSetStore = machinememory.NewBootSetStore()
	if f, ok := cfg.vmm(); ok {
		kernels, bootSets, bootFiles = f.offered(), f.bootSets(), pinnedBootFiles{}
		if addedSets, err = bootsetstore.New(filepath.Join(cfg.StateDir, "boot-sets")); err != nil {
			_ = db.Close()
			return nil, err
		}
		d, err := f.driver(cfg.Driver, supervisors)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		driver, guest = d, guestchannel.NewGuest(d)
		sub.Driver, sub.Guest = nil, nil
		capacity, sub.Capacity = hostcapacity.New(cfg.StateDir), nil
	}
	// The one allocator of VMM users: the lifecycle's Machines and the
	// conversions of pulls draw from it, so they never share a user.
	uids := machine.NewUIDs(cfg.VMMUIDRange.ToUIDRange())
	// Images: the real Registry, converter channel and Image Store replace the
	// substitutes under a real Driver, and the Image Store is what the
	// lifecycle locates an image in.
	var imgs images
	var located machine.ImageStore = sub.Images
	if cfg.Conversion != nil {
		var store machine.ImageStore
		imgs, store, err = realImages(cfg, clock, uids, pullTimeout)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		located, sub.Images = store, nil
	} else {
		imgs = memoryImages(sub, uids, pullTimeout)
	}
	// Disks and Snapshots: the real Disk and Snapshot Store replace the
	// substitutes under a real Driver.
	var disks machine.Disk = sub.Disks
	var snapshots machine.SnapshotStore = sub.Snapshots
	var diskSnapshots machine.DiskSnapshotStore = sub.DiskSnapshots
	if cfg.Disk != nil {
		disks, snapshots, diskSnapshots, err = realStorage(cfg, runDirs)
		if err != nil {
			_ = db.Close()
			return nil, err
		}
		sub.Disks, sub.Snapshots, sub.DiskSnapshots = nil, nil, nil
	}
	log := events.NewService(eventsKept)
	deadline := fencing.NewService(clock, db.Deadline(), supervisors, log)
	machines := machine.NewService(driver, deadline, network, disks, located, snapshots, guest, wallClock{}, db.Machines(), deadline, uids, log, machine.WithBootTimeout(bootTimeout), machine.WithCallTimeout(callTimeout), machine.WithKernels(kernels), machine.WithBootSets(bootSets, bootFiles), machine.WithAddedBootSets(addedSets), machine.WithDiskSnapshots(diskSnapshots), machine.WithBootIdentity(clock))

	machinesHeld := func() (uint32, error) {
		all, err := machines.List(context.Background())
		return uint32(len(all)), err
	}
	return &App{
		cfg:         cfg,
		db:          db,
		machines:    machines,
		deadline:    deadline,
		images:      imgs,
		server:      api.New(machines, deadline, imgs.service, driver.Info(), held.New(capacity, machineLimit, machinesHeld), log, api.WithKernels(kernels), api.WithBootSets(machines.ListBootSets), api.WithMachinePrefixes(prefixes...)),
		Substitutes: sub,
		ready:       make(chan struct{}),
	}, nil
}

// Ready is closed when the socket is listening, which is after the
// reconciliation.
func (a *App) Ready() <-chan struct{} { return a.ready }

// Serve ends the converter VMs a previous mvmd left, sweeps the Image Store
// (ADR 0007), restores the Machines from the state directory and reconciles
// the fencing deadline against them, then serves the runtime API on the
// socket until the context ends or the deadline's expiry loop fails, and
// watches for VMMs that end by themselves while it serves. No call
// is served before all of that has finished: the socket does not exist until
// then. It closes the database before it returns.
func (a *App) Serve(ctx context.Context) error {
	defer a.db.Close()
	if a.beforeReconcile != nil {
		a.beforeReconcile()
	}
	// The conversions go first: the sweep removes the staging area of one
	// whose VMM has ended, and no pull is running, so none is mistaken for a
	// leftover.
	if err := a.images.reap(ctx); err != nil {
		return fmt.Errorf("end the conversions of a previous mvmd: %w", err)
	}
	if err := a.images.service.Sweep(ctx); err != nil {
		return fmt.Errorf("sweep the Image Store: %w", err)
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
	wg.Add(3)
	go func() {
		defer wg.Done()
		a.machines.Watch(ctx)
	}()
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
