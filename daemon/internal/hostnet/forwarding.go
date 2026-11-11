package hostnet

import (
	"bytes"
	"fmt"
)

// The sysctls that must read 1 for a family the node serves. The host sets
// them; mvmd only reads them (runtime.md, Network).
const (
	ipv4ForwardingPath = "/proc/sys/net/ipv4/ip_forward"
	ipv6ForwardingPath = "/proc/sys/net/ipv6/conf/all/forwarding"
)

// ReadFile reads the file at a path, as os.ReadFile does. CheckForwarding takes
// it so that a test can stand in for /proc/sys.
type ReadFile func(path string) ([]byte, error)

// CheckForwarding refuses a node whose forwarding is off for a family its
// configuration serves, naming the sysctl. A family the configuration does not
// serve is not read. It sets nothing.
func (c Config) CheckForwarding(read ReadFile) error {
	for _, f := range []struct {
		served       bool
		family, path string
	}{
		{c.IPv4Prefix.IsValid(), "IPv4", ipv4ForwardingPath},
		{c.IPv6Prefix.IsValid(), "IPv6", ipv6ForwardingPath},
	} {
		if !f.served {
			continue
		}
		raw, err := read(f.path)
		if err != nil {
			return fmt.Errorf("the node serves %s and %s cannot be read: %w", f.family, f.path, err)
		}
		if value := bytes.TrimSpace(raw); string(value) != "1" {
			return fmt.Errorf("the node serves %s and %s reads %q, not 1: the host sets forwarding", f.family, f.path, value)
		}
	}
	return nil
}
