package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// DriverMemory selects the in-memory Driver, Supervision, Network, Disk,
// Snapshot Store and Image Store, and the in-memory Registry, converter and
// image cache behind PullImage. The real Supervision and Network are selected
// by their own section of the configuration, which replaces the substitute
// behind its port.
const DriverMemory = "memory"

// DriverFirecracker selects the Firecracker Driver, and the Guest Agent
// channel adapter over it, in place of the in-memory Driver and Guest, and
// with them the Image Store, the Registry and the converter channel behind
// PullImage, the Disk and the Snapshot Store. It needs the Firecracker,
// Conversion, Disk and Supervision sections.
const DriverFirecracker = "firecracker"

// DriverCloudHypervisor selects the Cloud Hypervisor Driver, and wires
// everything DriverFirecracker does over it. It needs the CloudHypervisor,
// Conversion, Disk and Supervision sections.
const DriverCloudHypervisor = "cloud-hypervisor"

// Config is mvmd's one configuration file.
type Config struct {
	Socket SocketConfig `json:"socket"`
	// StateDir holds mvmd's database. It is created if absent. Under the
	// real Drivers it also holds the Image Store, in "images", on a
	// filesystem with fs-verity enabled (image.md §Layout), and the state
	// directories of the converter VMs' supervisors, in "conversions", the
	// root disks, in "disks", and the snapshots, in "snapshots".
	StateDir string `json:"state_dir"`
	// Driver selects the adapters behind the ports: "memory", "firecracker" or
	// "cloud-hypervisor". A real Driver replaces the Driver, the Guest, the
	// Image Store, the Disk and the Snapshot Store with the real adapters, so a
	// Machine can be created, started, snapshotted and restored on the node. Root disks are
	// then in "disks" under StateDir and snapshots in "snapshots", which must
	// be on the same file system as the Machines' run directories.
	Driver string `json:"driver"`
	// Disk names what the Disk adapter needs. It is required by a real Driver
	// and refused with the memory one.
	Disk *DiskConfig `json:"disk,omitempty"`
	// Firecracker names the pinned binary, kernel and initrd the Firecracker
	// Driver boots; it is required by that Driver and refused with any other.
	Firecracker *VMMConfig `json:"firecracker,omitempty"`
	// CloudHypervisor names the pinned binary, kernel and initrd the Cloud
	// Hypervisor Driver boots; it is required by that Driver and refused with
	// any other.
	CloudHypervisor *VMMConfig `json:"cloud_hypervisor,omitempty"`
	// Conversion names what the converter VM of a pull needs: its kernel and
	// initrd, its size and its time limit. It is required by a real Driver and
	// refused with the memory one. The converter VM boots on the selected
	// Driver, with the binary of that Driver's section, and runs under a
	// supervisor of the supervision section, in a state directory and a VMM
	// cgroup of its own, so that it is not one of the Machines the fencing
	// deadline renews.
	Conversion *ConversionConfig `json:"conversion,omitempty"`
	// Registry names how the node authenticates to registries. It is refused
	// with the memory driver. Without it every pull is anonymous.
	Registry *RegistryConfig `json:"registry,omitempty"`
	// PullTimeout is how long a pull may run, as a Go duration such as "30m",
	// whether or not its caller waits. Empty means image.DefaultPullTimeout
	// (30m); it must be positive and at most MaxPullTimeout.
	PullTimeout string `json:"pull_timeout,omitempty"`
	// VMMUIDRange names the range of UIDs (and GIDs) reserved for Machines
	// and for the converter VMs of pulls, which draw from the one range
	// through the one allocator, so a conversion and a Machine never share a
	// user (image.md).
	// The first must be non-zero (root is forbidden) and Count must be
	// positive; the range must not overflow uint32 (ADR 0015).
	VMMUIDRange VMMUIDRangeConfig `json:"vmm_uid_range"`
	// Supervision, when set, starts a real mvmd-supervisor per Machine in place
	// of the in-memory substitute.
	Supervision *SupervisionConfig `json:"supervision,omitempty"`
	// Network makes a real tap per Machine and one nftables table in place of
	// the in-memory substitute, and mvmd then needs CAP_NET_ADMIN. A real
	// Driver requires it; the memory Driver may leave it out.
	Network *NetworkConfig `json:"network,omitempty"`
	// BootTimeout is how long Start waits for a guest to finish booting, as a
	// Go duration such as "90s". When it passes the VMM is ended and the
	// Machine is Stopped. Empty means machine.DefaultBootTimeout (2m); it must
	// be positive and at most MaxBootTimeout.
	BootTimeout string `json:"boot_timeout,omitempty"`
	// CallTimeout is how long an accepted state-changing call may run, as a Go
	// duration such as "5m", from the moment it holds its Machine; a start adds
	// the boot timeout and a stop its grace period. Empty means
	// machine.DefaultCallTimeout (10m); it must be positive and at most
	// MaxCallTimeout.
	CallTimeout string `json:"call_timeout,omitempty"`
}

// MaxBootTimeout is the longest boot timeout a node can name.
const MaxBootTimeout = time.Hour

// MaxCallTimeout is the longest call timeout a node can name.
const MaxCallTimeout = 6 * time.Hour

// MaxPullTimeout is the longest pull timeout a node can name.
const MaxPullTimeout = 6 * time.Hour

// MaxConversionLimit is the longest conversion time limit a node can name.
const MaxConversionLimit = time.Hour

// pullTimeout returns the configured bound, or the default when none is set.
func (c Config) pullTimeout() (time.Duration, error) {
	if c.PullTimeout == "" {
		return image.DefaultPullTimeout, nil
	}
	d, err := time.ParseDuration(c.PullTimeout)
	switch {
	case err != nil:
		return 0, fmt.Errorf("pull_timeout %q is not a duration such as \"30m\"", c.PullTimeout)
	case d <= 0 || d > MaxPullTimeout:
		return 0, fmt.Errorf("pull_timeout %q is outside 1ns to %s", c.PullTimeout, MaxPullTimeout)
	}
	return d, nil
}

// ConversionConfig is what the node says about a conversion (image.md
// §Conversion): the converter's kernel and initrd, by absolute path, the
// kernel and initrd also by SHA-256 in hex, which the Driver checks at start; the
// converter's name, which every image records; and its size and time limit.
// Nothing here has a default: a converter too small for an image fails the
// pull, and the right size depends on the images the node pulls.
type ConversionConfig struct {
	Kernel       string `json:"kernel"`
	KernelSHA256 string `json:"kernel_sha256"`
	Initrd       string `json:"initrd"`
	InitrdSHA256 string `json:"initrd_sha256"`
	// Converter names the converter; an image records the name it was
	// converted by.
	Converter string `json:"converter"`
	// VCPUs and MemoryBytes are the converter VM's. The memory has to exceed
	// twice the file data of the largest layer of an image to convert (ADR
	// 0017).
	VCPUs       uint32 `json:"vcpus"`
	MemoryBytes uint64 `json:"memory_bytes"`
	// OutputBytes is the size of the output file, the most an image can be.
	OutputBytes uint64 `json:"output_bytes"`
	// NameSetBytes is the most bytes of entry names the converter holds for
	// one layer.
	NameSetBytes uint64 `json:"name_set_bytes"`
	// Limit is how long a conversion may run from its VMM's start, as a Go
	// duration such as "10m"; it must be positive and at most
	// MaxConversionLimit. The supervisor's expiry is its end.
	Limit string `json:"limit"`
}

func (c ConversionConfig) limit() (time.Duration, error) {
	d, err := time.ParseDuration(c.Limit)
	switch {
	case err != nil:
		return 0, fmt.Errorf("conversion.limit %q is not a duration such as \"10m\"", c.Limit)
	case d <= 0 || d > MaxConversionLimit:
		return 0, fmt.Errorf("conversion.limit %q is outside 1ns to %s", c.Limit, MaxConversionLimit)
	}
	return d, nil
}

// check refuses a section that names no absolute path, no digest or no size.
// The converter VM boots with the binary of the selected Driver's section,
// which supplies the binary and its digest.
func (c ConversionConfig) check(driver string, vmm VMMConfig) error {
	if err := c.files(vmm).validate(driver); err != nil {
		return fmt.Errorf("conversion: %w", err)
	}
	switch {
	case c.Converter == "":
		return fmt.Errorf("conversion.converter is empty")
	case c.VCPUs == 0:
		return fmt.Errorf("conversion.vcpus is zero")
	case c.MemoryBytes == 0:
		return fmt.Errorf("conversion.memory_bytes is zero")
	case c.OutputBytes == 0:
		return fmt.Errorf("conversion.output_bytes is zero")
	case c.NameSetBytes == 0:
		return fmt.Errorf("conversion.name_set_bytes is zero")
	}
	_, err := c.limit()
	return err
}

// files are the converter's boot files with the selected Driver's binary.
func (c ConversionConfig) files(vmm VMMConfig) VMMConfig {
	return VMMConfig{
		Binary: vmm.Binary, BinarySHA256: vmm.BinarySHA256,
		Kernels:       map[string]KernelConfig{conversionKernel: {Path: c.Kernel, SHA256: c.KernelSHA256}},
		DefaultKernel: conversionKernel,
		Initrd:        c.Initrd, InitrdSHA256: c.InitrdSHA256,
	}
}

// settings is the image domain's view of the section.
func (c ConversionConfig) settings() (image.Settings, error) {
	limit, err := c.limit()
	if err != nil {
		return image.Settings{}, err
	}
	return image.Settings{
		Platform:     platform(),
		Converter:    c.Converter,
		Kernel:       machine.KernelFile{Path: c.Kernel, SHA256: c.KernelSHA256},
		Resources:    machine.Resources{VCPUs: c.VCPUs, MemoryBytes: c.MemoryBytes},
		OutputBytes:  c.OutputBytes,
		NameSetBytes: c.NameSetBytes,
		Limit:        limit,
	}, nil
}

// platform is the one the node selects manifests for: Linux on the CPU
// architecture mvmd runs on, which is the guests' (the Firecracker Driver
// boots only the host's).
func platform() image.Platform {
	return image.Platform{OS: "linux", Architecture: runtime.GOARCH}
}

// RegistryConfig is how the node authenticates to registries and which CA it
// trusts for their TLS. At least one key is set.
type RegistryConfig struct {
	// CredentialsFile is the absolute path of a JSON file mapping a registry,
	// as a repository names it (host, and :port when it has one), to an
	// object with "username" and "password". It is read at each credential
	// lookup of a pull, so a rewrite takes effect without a restart, and is
	// refused when it is not a regular file closed to group and others. The
	// secrets live there and in no other field, so the configuration can be
	// shared and logged without them.
	CredentialsFile string `json:"credentials_file,omitempty"`
	// CredentialHelpers maps a registry, named as in CredentialsFile, to the
	// name of a docker credential helper: a pull from that registry runs
	// docker-credential-<name> get, found on mvmd's PATH, at each lookup. A
	// registry named here and in CredentialsFile is refused.
	CredentialHelpers map[string]string `json:"credential_helpers,omitempty"`
	// CAFile is the absolute path of a PEM bundle added to the system roots
	// for registry TLS. It is read at each pull, and refused at start when it
	// is not a regular file or holds no certificate.
	CAFile string `json:"ca_file,omitempty"`
}

// callTimeout returns the configured bound, or the default when none is set.
func (c Config) callTimeout() (time.Duration, error) {
	if c.CallTimeout == "" {
		return machine.DefaultCallTimeout, nil
	}
	d, err := time.ParseDuration(c.CallTimeout)
	switch {
	case err != nil:
		return 0, fmt.Errorf("call_timeout %q is not a duration such as \"5m\"", c.CallTimeout)
	case d <= 0 || d > MaxCallTimeout:
		return 0, fmt.Errorf("call_timeout %q is outside 1ns to %s", c.CallTimeout, MaxCallTimeout)
	}
	return d, nil
}

// bootTimeout returns the configured bound, or the default when none is set.
// check has already refused a value that does not parse.
func (c Config) bootTimeout() (time.Duration, error) {
	if c.BootTimeout == "" {
		return machine.DefaultBootTimeout, nil
	}
	d, err := time.ParseDuration(c.BootTimeout)
	switch {
	case err != nil:
		return 0, fmt.Errorf("boot_timeout %q is not a duration such as \"90s\"", c.BootTimeout)
	case d <= 0 || d > MaxBootTimeout:
		return 0, fmt.Errorf("boot_timeout %q is outside 1ns to %s", c.BootTimeout, MaxBootTimeout)
	}
	return d, nil
}

// DiskConfig is what the node says about its root disks (ADR 0009).
type DiskConfig struct {
	// MKFS is the absolute path of the mkfs.ext4 that formats a root disk. It
	// is run with -V at start, and the node does not start when that fails.
	MKFS string `json:"mkfs"`
}

// VMMConfig is what the node says about the VMM of a real Driver and the guest
// it boots (ADR 0010, ADR 0030, ADR 0045): each file by absolute path, each
// also by SHA-256 in hex, which the Driver checks at start and the supervisor
// before every boot. Both real Drivers' sections have this shape.
type VMMConfig struct {
	Binary       string `json:"binary"`
	BinarySHA256 string `json:"binary_sha256"`
	// Kernels are the guest kernels the node offers, by name; a name is 1 to
	// 63 characters of lowercase letters, digits and "-". A Machine records
	// the name it is created with and boots the file the name maps to.
	Kernels map[string]KernelConfig `json:"kernels"`
	// DefaultKernel is the name a Create that names no kernel gets; it is one
	// of Kernels.
	DefaultKernel string `json:"default_kernel"`
	Initrd        string `json:"initrd"`
	InitrdSHA256  string `json:"initrd_sha256"`
}

// KernelConfig is one guest kernel: its absolute path and its SHA-256 in hex.
type KernelConfig struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// kernelName is what a kernel's name may be.
var kernelName = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

// conversionKernel is the name of the converter's one kernel in the section
// its Driver is built from; no Machine names it.
const conversionKernel = "conversion"

// kernelFiles are the kernels in the order of their names.
func (c VMMConfig) kernelFiles() []machine.KernelFile {
	files := make([]machine.KernelFile, 0, len(c.Kernels))
	for _, name := range slices.Sorted(maps.Keys(c.Kernels)) {
		files = append(files, machine.KernelFile{Path: c.Kernels[name].Path, SHA256: c.Kernels[name].SHA256})
	}
	return files
}

// offered is the kernels as the lifecycle resolves them.
func (c VMMConfig) offered() machine.Kernels {
	offered := make(map[string]machine.KernelFile, len(c.Kernels))
	for name, k := range c.Kernels {
		offered[name] = machine.KernelFile{Path: k.Path, SHA256: k.SHA256}
	}
	return machine.Kernels{Default: c.DefaultKernel, Offered: offered}
}

// checkKernels refuses a section without a kernel, with a bad name, or whose
// default is not one of its kernels. It names the field.
func (c VMMConfig) checkKernels() error {
	if len(c.Kernels) == 0 {
		return fmt.Errorf("kernels is empty: a node offers at least one kernel")
	}
	for _, name := range slices.Sorted(maps.Keys(c.Kernels)) {
		if !kernelName.MatchString(name) {
			return fmt.Errorf("kernels name %q is not 1 to 63 characters of a-z, 0-9 and \"-\"", name)
		}
	}
	if _, ok := c.Kernels[c.DefaultKernel]; !ok {
		return fmt.Errorf("default_kernel %q is not a name in kernels", c.DefaultKernel)
	}
	return nil
}

// firecracker names the Firecracker Driver's files; it adds the run directories.
func (c VMMConfig) firecracker(dirs firecracker.RunDirs) firecracker.Config {
	return firecracker.Config{
		Binary: c.Binary, BinarySHA256: c.BinarySHA256,
		Kernels: c.kernelFiles(),
		Initrd:  c.Initrd, InitrdSHA256: c.InitrdSHA256, Dirs: dirs,
	}
}

// cloudHypervisor names the Cloud Hypervisor Driver's files; it adds the run
// directories.
func (c VMMConfig) cloudHypervisor(dirs cloudhypervisor.RunDirs) cloudhypervisor.Config {
	return cloudhypervisor.Config{
		Binary: c.Binary, BinarySHA256: c.BinarySHA256,
		Kernels: c.kernelFiles(),
		Initrd:  c.Initrd, InitrdSHA256: c.InitrdSHA256, Dirs: dirs,
	}
}

// vmm is the section of the selected real Driver; it is false under the
// memory driver.
func (c Config) vmm() (VMMConfig, bool) {
	switch {
	case c.Firecracker != nil:
		return *c.Firecracker, true
	case c.CloudHypervisor != nil:
		return *c.CloudHypervisor, true
	}
	return VMMConfig{}, false
}

// realDriver is what both real Drivers are: the Driver port, and the
// connector the Guest Agent's and the converter's channels reach a guest by.
type realDriver interface {
	machine.Driver
	guestchannel.Connector
}

// validate runs the named real Driver's own check over these files; it reads
// no file. Any other name is an error.
func (c VMMConfig) validate(name string) error {
	if err := c.checkKernels(); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	switch name {
	case DriverFirecracker:
		return c.firecracker(nil).Validate()
	case DriverCloudHypervisor:
		return c.cloudHypervisor(nil).Validate()
	}
	return fmt.Errorf("driver %q is not a real driver", name)
}

// driver is the named real Driver over these files; any other name is an
// error. The Driver checks every file against its digest.
func (c VMMConfig) driver(name string, dirs fencing.Supervisors) (realDriver, error) {
	switch name {
	case DriverFirecracker:
		return firecracker.New(c.firecracker(dirs))
	case DriverCloudHypervisor:
		return cloudhypervisor.New(c.cloudHypervisor(dirs))
	}
	return nil, fmt.Errorf("driver %q is not a real driver", name)
}

// NetworkConfig is what the node says about its network (ADR 0011). A node
// serves the families it names a prefix for.
type NetworkConfig struct {
	// IPv4Prefix is the node's IPv4 prefix, such as "10.77.0.0/24"; empty
	// means the node does not serve IPv4.
	IPv4Prefix string `json:"ipv4_prefix"`
	// IPv4Gateway is the host side of every tap, inside IPv4Prefix.
	IPv4Gateway string `json:"ipv4_gateway"`
	// IPv6Prefix is the node's IPv6 prefix, such as "fd77::/64"; empty means
	// the node does not serve IPv6.
	IPv6Prefix string `json:"ipv6_prefix"`
	// Resolvers are the addresses the guest is told to resolve names with.
	Resolvers []string `json:"resolvers"`
	// NoMasquerade are CIDRs, in the families the node serves, that traffic
	// from a Machine reaches with the Machine's own source address (ADR 0049).
	NoMasquerade []string `json:"no_masquerade"`
}

// toHostnet parses the node's network into the adapter's configuration.
func (c NetworkConfig) toHostnet(uids VMMUIDRangeConfig) (hostnet.Config, error) {
	out := hostnet.Config{VMMUIDRange: uids.ToUIDRange()}
	var err error
	if c.IPv4Prefix != "" {
		if out.IPv4Prefix, err = netip.ParsePrefix(c.IPv4Prefix); err != nil {
			return out, fmt.Errorf("network.ipv4_prefix: %w", err)
		}
		if out.IPv4Gateway, err = netip.ParseAddr(c.IPv4Gateway); err != nil {
			return out, fmt.Errorf("network.ipv4_gateway: %w", err)
		}
	} else if c.IPv4Gateway != "" {
		return out, fmt.Errorf("network.ipv4_gateway is set and network.ipv4_prefix is not")
	}
	if c.IPv6Prefix != "" {
		if out.IPv6Prefix, err = netip.ParsePrefix(c.IPv6Prefix); err != nil {
			return out, fmt.Errorf("network.ipv6_prefix: %w", err)
		}
	}
	for _, r := range c.Resolvers {
		a, err := netip.ParseAddr(r)
		if err != nil {
			return out, fmt.Errorf("network.resolvers: %w", err)
		}
		out.Resolvers = append(out.Resolvers, a)
	}
	for _, s := range c.NoMasquerade {
		p, err := netip.ParsePrefix(s)
		if err != nil {
			return out, fmt.Errorf("network.no_masquerade: %w", err)
		}
		out.NoMasquerade = append(out.NoMasquerade, p)
	}
	return out, out.Validate()
}

// VMMUIDRangeConfig is the JSON-marshaled form of machine.UIDRange.
type VMMUIDRangeConfig struct {
	First uint32 `json:"first"`
	Count uint32 `json:"count"`
}

// ToUIDRange converts to the machine.UIDRange type.
func (c VMMUIDRangeConfig) ToUIDRange() machine.UIDRange {
	return machine.UIDRange{First: c.First, Count: c.Count}
}

// SupervisionConfig places the per-Machine supervisors (ADR 0003).
type SupervisionConfig struct {
	// Binary is the absolute path of mvmd-supervisor.
	Binary string `json:"binary"`
	// Dir holds one state directory per Machine, with the supervisor's socket
	// in it. Empty is "machines" under the state directory. The directory is
	// the Machines' supervision's alone: a start removes any directory in it
	// whose Machine the State Store does not hold, so it is neither the state
	// directory, nor an ancestor of it, nor inside it other than "machines".
	Dir string `json:"dir"`
	// Cgroup is a cgroup v2 directory the supervisors start in, outside
	// mvmd's cgroup. Empty leaves them in mvmd's.
	Cgroup string `json:"cgroup"`
	// VMMCgroup is the cgroup v2 parent under which each VMM gets a cgroup
	// of its own (ADR 0015). It must be absolute when given; empty
	// defaults to the adapter's default.
	VMMCgroup string `json:"vmm_cgroup"`
}

// SocketConfig is where the runtime API is served and who may reach it: the
// socket's file permissions are the authentication (ADR 0004).
type SocketConfig struct {
	Path string `json:"path"`
	// Mode is the socket's permission bits, in octal, such as "0660".
	Mode string `json:"mode"`
	// Group owns the socket. Empty leaves the group of the process.
	Group string `json:"group"`
}

// LoadConfig reads and checks the configuration file. A field it does not
// know is an error, so a misspelt option is not silently ignored.
func LoadConfig(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read the configuration: %w", err)
	}
	return ParseConfig(raw)
}

// ParseConfig parses and checks a configuration.
func ParseConfig(raw []byte) (Config, error) {
	var c Config
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return Config{}, fmt.Errorf("parse the configuration: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return Config{}, fmt.Errorf("parse the configuration: trailing data after the object")
	}
	if err := c.check(); err != nil {
		return Config{}, fmt.Errorf("configuration: %w", err)
	}
	return c, nil
}

func (c Config) check() error {
	switch {
	case !filepath.IsAbs(c.Socket.Path):
		return fmt.Errorf("socket.path %q is not an absolute path", c.Socket.Path)
	case !filepath.IsAbs(c.StateDir):
		return fmt.Errorf("state_dir %q is not an absolute path", c.StateDir)
	case c.Driver != DriverMemory && c.Driver != DriverFirecracker && c.Driver != DriverCloudHypervisor:
		return fmt.Errorf("driver %q is not one of %q", c.Driver, []string{DriverMemory, DriverFirecracker, DriverCloudHypervisor})
	case c.Driver == DriverFirecracker && c.Firecracker == nil:
		return fmt.Errorf("driver %q needs the firecracker section", c.Driver)
	case c.Driver == DriverCloudHypervisor && c.CloudHypervisor == nil:
		return fmt.Errorf("driver %q needs the cloud_hypervisor section", c.Driver)
	case c.Driver != DriverFirecracker && c.Firecracker != nil:
		return fmt.Errorf("the firecracker section is for driver %q, not %q", DriverFirecracker, c.Driver)
	case c.Driver != DriverCloudHypervisor && c.CloudHypervisor != nil:
		return fmt.Errorf("the cloud_hypervisor section is for driver %q, not %q", DriverCloudHypervisor, c.Driver)
	case c.Driver != DriverMemory && c.Supervision == nil:
		return fmt.Errorf("driver %q needs the supervision section: its VMM runs under a supervisor", c.Driver)
	case c.Driver != DriverMemory && c.Conversion == nil:
		return fmt.Errorf("driver %q needs the conversion section: PullImage converts in a converter VM", c.Driver)
	case c.Driver != DriverMemory && c.Disk == nil:
		return fmt.Errorf("driver %q needs the disk section: a Machine's root disk is formatted by mkfs.ext4", c.Driver)
	case c.Driver != DriverMemory && c.Network == nil:
		return fmt.Errorf("driver %q needs the network section: a Machine starts only once its network policy is applied, and without one its VMM is refused a tap that was never made", c.Driver)
	case c.Driver == DriverMemory && c.Disk != nil:
		return fmt.Errorf("the disk section is for a real driver, not %q", c.Driver)
	case c.Driver == DriverMemory && c.Conversion != nil:
		return fmt.Errorf("the conversion section is for a real driver, not %q", c.Driver)
	case c.Driver == DriverMemory && c.Registry != nil:
		return fmt.Errorf("the registry section is for a real driver, not %q", c.Driver)
	}
	if f := c.Firecracker; f != nil {
		if err := f.validate(DriverFirecracker); err != nil {
			return err
		}
	}
	if h := c.CloudHypervisor; h != nil {
		if err := h.validate(DriverCloudHypervisor); err != nil {
			return err
		}
	}
	if cv := c.Conversion; cv != nil {
		vmm, _ := c.vmm()
		if err := cv.check(c.Driver, vmm); err != nil {
			return err
		}
	}
	if d := c.Disk; d != nil && !filepath.IsAbs(d.MKFS) {
		return fmt.Errorf("disk.mkfs %q is not an absolute path", d.MKFS)
	}
	if r := c.Registry; r != nil {
		if err := r.validate(); err != nil {
			return err
		}
	}
	if _, err := c.pullTimeout(); err != nil {
		return err
	}
	if _, err := c.Socket.perm(); err != nil {
		return err
	}
	if err := c.VMMUIDRange.ToUIDRange().Validate(); err != nil {
		return fmt.Errorf("vmm_uid_range: %w", err)
	}
	if _, err := c.bootTimeout(); err != nil {
		return err
	}
	if _, err := c.callTimeout(); err != nil {
		return err
	}
	if n := c.Network; n != nil {
		if _, err := n.toHostnet(c.VMMUIDRange); err != nil {
			return err
		}
	}
	if s := c.Supervision; s != nil {
		switch {
		case !filepath.IsAbs(s.Binary):
			return fmt.Errorf("supervision.binary %q is not an absolute path", s.Binary)
		case s.Dir != "" && !filepath.IsAbs(s.Dir):
			return fmt.Errorf("supervision.dir %q is not an absolute path", s.Dir)
		case s.Dir != "" && sharesStateDir(s.Dir, c.StateDir):
			return fmt.Errorf("supervision.dir %q conflicts with state_dir %q: it must be \"machines\" under it or a path outside it, because a start removes every directory in it that no Machine owns", s.Dir, c.StateDir)
		case s.Cgroup != "" && !filepath.IsAbs(s.Cgroup):
			return fmt.Errorf("supervision.cgroup %q is not an absolute path", s.Cgroup)
		case s.VMMCgroup != "" && !filepath.IsAbs(s.VMMCgroup):
			return fmt.Errorf("supervision.vmm_cgroup %q is not an absolute path", s.VMMCgroup)
		}
	}
	return nil
}

// leavesBase reports whether a relative path climbs out of its base, which a
// first component such as "..data" does not.
func leavesBase(rel string) bool {
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// sharesStateDir reports whether dir is the state directory, an ancestor of
// it, or inside it other than at "machines".
func sharesStateDir(dir, stateDir string) bool {
	dir, stateDir = filepath.Clean(dir), filepath.Clean(stateDir)
	if rel, err := filepath.Rel(dir, stateDir); err == nil && !leavesBase(rel) {
		return true // dir is the state directory or an ancestor of it
	}
	rel, err := filepath.Rel(stateDir, dir)
	return err == nil && !leavesBase(rel) && rel != "machines"
}

func (s SocketConfig) perm() (os.FileMode, error) {
	mode, err := strconv.ParseUint(s.Mode, 8, 32)
	if err != nil || mode > 0o777 {
		return 0, fmt.Errorf("socket.mode %q is not an octal permission such as \"0660\"", s.Mode)
	}
	return os.FileMode(mode), nil
}
