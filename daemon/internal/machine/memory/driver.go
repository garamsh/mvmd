package memory

import (
	"context"
	"errors"
	"io"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Driver is an in-memory machine.Driver. Its steps are "describe", "boot",
// "guest-conn", "console", "console-log",
// "shutdown", "pause", "resume", "attach", "detach", "drive", "resize", "snapshot",
// "describe-restore" and "restore".
type Driver struct {
	Failures
	info machine.DriverInfo

	mu       sync.Mutex
	specs    map[machine.MachineID]machine.BootSpec
	booted   map[machine.MachineID]machine.BootSpec
	grace    time.Duration
	devices  map[machine.MachineID][]machine.DiskID
	sizes    map[machine.MachineID]machine.Resources
	restored map[machine.MachineID][]string
	// restoreSpec is the boot description each Machine's Restore was given.
	restoreSpec map[machine.MachineID]machine.BootSpec
	// running tracks Machines booted or restored until Shutdown clears them.
	running map[machine.MachineID]bool
	paused  map[machine.MachineID]bool
	// pausedAtSnapshot is what paused was when the Machine was last snapshotted.
	pausedAtSnapshot map[machine.MachineID]bool
	hold             chan struct{}
	entered          chan struct{}
	process          *machine.ProcessDescription
	report           func(machine.DriverEvent)
	guests           map[guestPort]func(net.Conn)
	consoles         map[machine.MachineID]func(io.ReadWriteCloser)
	noConsole        bool
	logs             map[machine.MachineID]string
}

// Observe implements machine.Driver.
func (d *Driver) Observe(report func(machine.DriverEvent)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.report = report
}

// Raise makes the Driver report an event of its own, as a VMM that exited
// does. It does nothing before Observe.
func (d *Driver) Raise(name, detail string) {
	d.mu.Lock()
	report := d.report
	d.mu.Unlock()
	if report != nil {
		report(machine.DriverEvent{Name: name, Detail: detail})
	}
}

// UseProcess makes Describe return the process instead of the fixed one with
// the Machine's boot files, for a test that starts a real process under a real
// supervisor.
func (d *Driver) UseProcess(p machine.ProcessDescription) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.process = &p
}

// NewDriver returns a Driver reporting the given name, version and
// capabilities.
func NewDriver(info machine.DriverInfo) *Driver {
	return &Driver{
		info:             info,
		specs:            make(map[machine.MachineID]machine.BootSpec),
		booted:           make(map[machine.MachineID]machine.BootSpec),
		devices:          make(map[machine.MachineID][]machine.DiskID),
		sizes:            make(map[machine.MachineID]machine.Resources),
		restored:         make(map[machine.MachineID][]string),
		restoreSpec:      make(map[machine.MachineID]machine.BootSpec),
		paused:           make(map[machine.MachineID]bool),
		pausedAtSnapshot: make(map[machine.MachineID]bool),
		running:          make(map[machine.MachineID]bool),
	}
}

// ServeGuest makes the guest of the Machine answer on the port with serve: each
// GuestConn to it runs serve on its own goroutine, with the guest's end of a
// new in-process stream, which serve owns and closes. It stands in for a guest
// whose Guest Agent listens on the port.
func (d *Driver) ServeGuest(id machine.MachineID, port uint32, serve func(net.Conn)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.guests == nil {
		d.guests = make(map[guestPort]func(net.Conn))
	}
	d.guests[guestPort{id, port}] = serve
}

type guestPort struct {
	id   machine.MachineID
	port uint32
}

// GuestConn implements machine.Driver. Its step is "guest-conn". A Machine
// and port nothing serves is machine.ErrGuestUnreachable when the Machine is
// running (from Boot or Restore until Shutdown) and machine.ErrVMMEnded
// joined with machine.ErrGuestUnreachable when it is not; an injected
// failure returns machine.ErrGuestUnreachable joined with the injected error.
func (d *Driver) GuestConn(_ context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	if err := d.Enter("guest-conn"); err != nil {
		return nil, errors.Join(machine.ErrGuestUnreachable, err)
	}
	d.mu.Lock()
	serve, ok := d.guests[guestPort{id, port}]
	running := d.running[id]
	d.mu.Unlock()
	if !ok && !running {
		return nil, errors.Join(machine.ErrVMMEnded, machine.ErrGuestUnreachable)
	}
	if !ok {
		return nil, machine.ErrGuestUnreachable
	}
	host, guest := net.Pipe()
	go serve(guest)
	return host, nil
}

// ServeConsole makes the console of the Machine answer with serve: each Console
// of it runs serve on its own goroutine, with the guest's end of a new
// in-process stream, which serve owns and closes. It stands in for a guest
// whose console is attached to the session.
func (d *Driver) ServeConsole(id machine.MachineID, serve func(io.ReadWriteCloser)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.consoles == nil {
		d.consoles = make(map[machine.MachineID]func(io.ReadWriteCloser))
	}
	d.consoles[id] = serve
}

// Console implements machine.Driver. Its step is "console". A Machine nothing
// serves is machine.ErrConsoleUnavailable; an injected failure is returned as
// it was told, so a test names machine.ErrConsoleUnavailable to inject that one.
func (d *Driver) Console(ctx context.Context, id machine.MachineID) (io.ReadWriteCloser, error) {
	if err := d.Enter("console"); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.Lock()
	serve, ok := d.consoles[id]
	d.mu.Unlock()
	if !ok {
		return nil, machine.ErrConsoleUnavailable
	}
	host, guest := net.Pipe()
	go serve(guest)
	return host, nil
}

// WriteConsoleLog sets what the console log of the Machine holds.
func (d *Driver) WriteConsoleLog(id machine.MachineID, content string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.logs == nil {
		d.logs = make(map[machine.MachineID]string)
	}
	d.logs[id] = content
}

// ConsoleLog implements machine.Driver. Its step is "console-log". A Machine
// whose log was never written is machine.ErrConsoleUnavailable, and so is an
// injected failure, unless it is told another error.
func (d *Driver) ConsoleLog(_ context.Context, id machine.MachineID) (io.ReadCloser, error) {
	if err := d.Enter("console-log"); err != nil {
		return nil, errors.Join(machine.ErrConsoleUnavailable, err)
	}
	d.mu.Lock()
	content, ok := d.logs[id]
	d.mu.Unlock()
	if !ok {
		return nil, machine.ErrConsoleUnavailable
	}
	return io.NopCloser(strings.NewReader(content)), nil
}

// Info implements machine.Driver. The Driver also reports the console
// capability, unless WithoutConsole was called.
func (d *Driver) Info() machine.DriverInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	info := d.info
	info.Capabilities = append([]machine.Capability(nil), info.Capabilities...)
	if !d.noConsole {
		info.Capabilities = append(info.Capabilities, machine.CapabilityConsole)
	}
	return info
}

// WithoutConsole makes the Driver stop reporting the console capability, as a
// VMM Driver that cannot open a console session does.
func (d *Driver) WithoutConsole() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.noConsole = true
}

// Describe implements machine.Driver.
func (d *Driver) Describe(_ context.Context, id machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	if err := d.Enter("describe"); err != nil {
		return machine.ProcessDescription{}, err
	}
	d.mu.Lock()
	d.specs[id] = spec
	p := d.process
	d.mu.Unlock()
	if p != nil {
		return *p, nil
	}
	kernel := machine.File{Host: "/memory/kernel", In: machine.InKernel, ReadOnly: true, SHA256: KernelDigest}
	return machine.ProcessDescription{Executable: "/usr/libexec/" + d.info.Name, BootFiles: append([]machine.File{kernel}, machine.DeviceFiles(spec.Devices)...)}, nil
}

// Boot implements machine.Driver.
func (d *Driver) Boot(_ context.Context, id machine.MachineID, spec machine.BootSpec) error {
	d.mu.Lock()
	d.booted[id] = spec
	d.running[id] = true
	d.mu.Unlock()
	return d.Enter("boot")
}

// BootedWith returns the boot description the last Boot of the Machine was
// given.
func (d *Driver) BootedWith(id machine.MachineID) machine.BootSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.booted[id]
}

// Shutdown implements machine.Driver.
func (d *Driver) Shutdown(_ context.Context, id machine.MachineID, grace time.Duration) error {
	d.mu.Lock()
	d.grace = grace
	d.running[id] = false
	hold, entered := d.hold, d.entered
	d.mu.Unlock()
	if hold != nil {
		close(entered)
		<-hold
	}
	return d.Enter("shutdown")
}

// HoldShutdown makes the next Shutdown wait, as a guest does for its grace
// period. entered is closed when a Shutdown is waiting; release lets it return.
func (d *Driver) HoldShutdown() (entered <-chan struct{}, release func()) {
	hold, ch := make(chan struct{}), make(chan struct{})
	d.mu.Lock()
	d.hold, d.entered = hold, ch
	d.mu.Unlock()
	return ch, func() { close(hold) }
}

// Pause implements machine.Driver.
func (d *Driver) Pause(_ context.Context, id machine.MachineID) error {
	if err := d.Enter("pause"); err != nil {
		return err
	}
	d.setPaused(id, true)
	return nil
}

// Resume implements machine.Driver.
func (d *Driver) Resume(_ context.Context, id machine.MachineID) error {
	if err := d.Enter("resume"); err != nil {
		return err
	}
	d.setPaused(id, false)
	return nil
}

func (d *Driver) setPaused(id machine.MachineID, paused bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused[id] = paused
}

// Paused reports whether the Machine's guest is paused as far as the Driver
// knows.
func (d *Driver) Paused(id machine.MachineID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.paused[id]
}

// PausedAtSnapshot reports whether the guest was paused when the last Snapshot
// of the Machine was taken.
func (d *Driver) PausedAtSnapshot(id machine.MachineID) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.pausedAtSnapshot[id]
}

// AttachDisk implements machine.Driver.
func (d *Driver) AttachDisk(_ context.Context, id machine.MachineID, dev machine.BlockDevice) error {
	if err := d.Enter("attach"); err != nil {
		return err
	}
	d.mu.Lock()
	d.devices[id] = append(d.devices[id], dev.Disk)
	d.mu.Unlock()
	return nil
}

// DetachDisk implements machine.Driver.
func (d *Driver) DetachDisk(_ context.Context, id machine.MachineID, disk machine.DiskID) error {
	if err := d.Enter("detach"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := d.devices[id][:0]
	for _, have := range d.devices[id] {
		if have != disk {
			kept = append(kept, have)
		}
	}
	d.devices[id] = kept
	return nil
}

// restoredOwnDevices is how many of a restore's devices are the Machine's own:
// its image and its root disk. The rest are the disks the snapshot recorded.
const restoredOwnDevices = 2

// Drive implements machine.Driver. Its step is "drive". The Machine's boot
// description gives its drives at boot, and AttachDisk the ones it has since.
func (d *Driver) Drive(_ context.Context, id machine.MachineID, disk machine.DiskID) (machine.DriveState, error) {
	if err := d.Enter("drive"); err != nil {
		return machine.DriveAbsent, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if slices.Contains(d.devices[id], disk) {
		return machine.DriveHotPlugged, nil
	}
	// The drives a Machine has at boot are those of the description it was
	// booted with, or restored with.
	spec, known := d.booted[id]
	restored := false
	if !known {
		spec, known = d.restoreSpec[id]
		restored = known
	}
	// A Machine this Driver neither booted nor restored is one a new process
	// inherited with its VMM, whose drives the substitute does not carry over:
	// it reports the disk as the boot gave it, which the restart keeps and does
	// not ask the guest of.
	if !known {
		return machine.DriveAtBoot, nil
	}
	for i, dev := range spec.Devices {
		if dev.Disk != disk {
			continue
		}
		// A restored VMM holds a recorded disk, any device after the image and
		// the root disk, as a hot-plugged one (ADR 0041).
		if restored && i >= restoredOwnDevices {
			return machine.DriveHotPlugged, nil
		}
		return machine.DriveAtBoot, nil
	}
	return machine.DriveAbsent, nil
}

// Resize implements machine.Driver.
func (d *Driver) Resize(_ context.Context, id machine.MachineID, target machine.Resources) error {
	if err := d.Enter("resize"); err != nil {
		return err
	}
	d.mu.Lock()
	d.sizes[id] = target
	d.mu.Unlock()
	return nil
}

// KernelDigest is the digest of the guest kernel the Driver names in the boot
// description of a start and reports in every snapshot.
const KernelDigest = "sha256:memory-kernel"

// Snapshot implements machine.Driver: two files in the Machine's run directory.
func (d *Driver) Snapshot(_ context.Context, id machine.MachineID) (machine.DriverSnapshot, error) {
	if err := d.Enter("snapshot"); err != nil {
		return machine.DriverSnapshot{}, err
	}
	d.mu.Lock()
	run := d.specs[id].RunDir
	d.pausedAtSnapshot[id] = d.paused[id]
	d.mu.Unlock()
	return machine.DriverSnapshot{Files: []string{run + "/snapshot.mem", run + "/snapshot.state"}, KernelDigest: KernelDigest}, nil
}

// DescribeRestore implements machine.Driver.
func (d *Driver) DescribeRestore(_ context.Context, id machine.MachineID, spec machine.BootSpec, files []string) (machine.ProcessDescription, error) {
	if err := d.Enter("describe-restore"); err != nil {
		return machine.ProcessDescription{}, err
	}
	d.mu.Lock()
	d.specs[id] = spec
	d.restored[id] = append([]string(nil), files...)
	p := d.process
	d.mu.Unlock()
	if p != nil {
		return *p, nil
	}
	return machine.ProcessDescription{Executable: "/usr/libexec/" + d.info.Name, BootFiles: machine.DeviceFiles(spec.Devices)}, nil
}

// Restore implements machine.Driver.
func (d *Driver) Restore(_ context.Context, id machine.MachineID, spec machine.BootSpec) error {
	if err := d.Enter("restore"); err != nil {
		return err
	}
	d.mu.Lock()
	d.restoreSpec[id] = spec
	d.running[id] = true
	d.mu.Unlock()
	return nil
}

// RestoredWith returns the boot description the last Restore for the Machine
// was given.
func (d *Driver) RestoredWith(id machine.MachineID) machine.BootSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.restoreSpec[id]
}

// RestoredFrom returns the snapshot files the last DescribeRestore for the
// Machine was given.
func (d *Driver) RestoredFrom(id machine.MachineID) []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.restored[id]...)
}

// BootSpec returns the boot description the last Describe for the Machine got.
func (d *Driver) BootSpec(id machine.MachineID) machine.BootSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[id]
}

// Grace returns the grace period of the last Shutdown.
func (d *Driver) Grace() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.grace
}

// HotDisks returns the disks the Driver attached to the running Machine.
func (d *Driver) HotDisks(id machine.MachineID) []machine.DiskID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]machine.DiskID(nil), d.devices[id]...)
}

// Resized returns the target of the last Resize of the Machine.
func (d *Driver) Resized(id machine.MachineID) machine.Resources {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sizes[id]
}
