package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// DriverMemory selects the in-memory Driver, Supervision, Network, Disk and
// Image Store. The real adapters join the choice with their own issues.
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
	return nil
}

func (s SocketConfig) perm() (os.FileMode, error) {
	mode, err := strconv.ParseUint(s.Mode, 8, 32)
	if err != nil || mode > 0o777 {
		return 0, fmt.Errorf("socket.mode %q is not an octal permission such as \"0660\"", s.Mode)
	}
	return os.FileMode(mode), nil
}
