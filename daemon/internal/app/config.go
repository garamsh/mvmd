package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// DriverMemory selects the in-memory Driver, Supervision, Network, Disk and
// Image Store. The real adapters join the choice with their own issues; the
// ones that exist are selected by their own section of the configuration
// (Supervision, Network), which replaces the substitute behind its port.
const DriverMemory = "memory"

// Config is mvmd's one configuration file.
type Config struct {
	Socket SocketConfig `json:"socket"`
	// StateDir holds mvmd's database. It is created if absent.
	StateDir string `json:"state_dir"`
	// Driver selects the adapters behind the ports: "memory".
	Driver string `json:"driver"`
	// VMMUIDRange names the range of UIDs (and GIDs) reserved for Machines.
	// The first must be non-zero (root is forbidden) and Count must be
	// positive; the range must not overflow uint32 (ADR 0015).
	VMMUIDRange VMMUIDRangeConfig `json:"vmm_uid_range"`
	// Supervision, when set, starts a real mvmd-supervisor per Machine in place
	// of the in-memory substitute.
	Supervision *SupervisionConfig `json:"supervision,omitempty"`
	// Network, when set, makes a real tap per Machine and one nftables table
	// in place of the in-memory substitute. mvmd then needs CAP_NET_ADMIN.
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
	// in it. Empty is "machines" under the state directory.
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
	case c.Driver != DriverMemory:
		return fmt.Errorf("driver %q is not one of %q", c.Driver, DriverMemory)
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
		case s.Cgroup != "" && !filepath.IsAbs(s.Cgroup):
			return fmt.Errorf("supervision.cgroup %q is not an absolute path", s.Cgroup)
		case s.VMMCgroup != "" && !filepath.IsAbs(s.VMMCgroup):
			return fmt.Errorf("supervision.vmm_cgroup %q is not an absolute path", s.VMMCgroup)
		}
	}
	return nil
}

func (s SocketConfig) perm() (os.FileMode, error) {
	mode, err := strconv.ParseUint(s.Mode, 8, 32)
	if err != nil || mode > 0o777 {
		return 0, fmt.Errorf("socket.mode %q is not an octal permission such as \"0660\"", s.Mode)
	}
	return os.FileMode(mode), nil
}
