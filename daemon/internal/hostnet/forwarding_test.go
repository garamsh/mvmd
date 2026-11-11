package hostnet_test

import (
	"io/fs"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/hostnet"
)

const (
	ipv4Forwarding = "/proc/sys/net/ipv4/ip_forward"
	ipv6Forwarding = "/proc/sys/net/ipv6/conf/all/forwarding"
)

// sysctls is a /proc/sys that holds the values it is given, and records which
// paths were read.
type sysctls struct {
	values map[string]string
	read   []string
}

func (s *sysctls) ReadFile(path string) ([]byte, error) {
	s.read = append(s.read, path)
	v, ok := s.values[path]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: path, Err: fs.ErrNotExist}
	}
	return []byte(v + "\n"), nil
}

// A node refuses to start when forwarding is off for a family it serves, and
// the error names the sysctl; a family it does not serve is not read.
func TestCheckForwarding(t *testing.T) {
	for _, tc := range []struct {
		name     string
		cfg      hostnet.Config
		values   map[string]string
		wantErr  string
		wantRead []string
	}{
		{
			name: "IPv4 served, ip_forward 0", cfg: ipv4Only(),
			values:   map[string]string{ipv4Forwarding: "0", ipv6Forwarding: "1"},
			wantErr:  ipv4Forwarding,
			wantRead: []string{ipv4Forwarding},
		},
		{
			name: "IPv6 served, conf/all/forwarding 0", cfg: ipv6Only(),
			values:   map[string]string{ipv4Forwarding: "1", ipv6Forwarding: "0"},
			wantErr:  ipv6Forwarding,
			wantRead: []string{ipv6Forwarding},
		},
		{
			name: "both served, both 1", cfg: dualStack(),
			values:   map[string]string{ipv4Forwarding: "1", ipv6Forwarding: "1"},
			wantRead: []string{ipv4Forwarding, ipv6Forwarding},
		},
		{
			name: "both served, only IPv6 off", cfg: dualStack(),
			values:   map[string]string{ipv4Forwarding: "1", ipv6Forwarding: "0"},
			wantErr:  ipv6Forwarding,
			wantRead: []string{ipv4Forwarding, ipv6Forwarding},
		},
		{
			name: "IPv4 served, IPv6 not served and 0", cfg: ipv4Only(),
			values:   map[string]string{ipv4Forwarding: "1", ipv6Forwarding: "0"},
			wantRead: []string{ipv4Forwarding},
		},
		{
			name: "IPv6 served, IPv4 not served and 0", cfg: ipv6Only(),
			values:   map[string]string{ipv4Forwarding: "0", ipv6Forwarding: "1"},
			wantRead: []string{ipv6Forwarding},
		},
		{
			name: "IPv4 served, sysctl unreadable", cfg: ipv4Only(),
			values:   map[string]string{},
			wantErr:  ipv4Forwarding,
			wantRead: []string{ipv4Forwarding},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &sysctls{values: tc.values}
			err := tc.cfg.CheckForwarding(s.ReadFile)
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			assert.Equal(t, tc.wantRead, s.read, "the paths read are those of the families served")
		})
	}
}
