package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

const goodConfig = `{"socket":{"path":"/run/mvmd.sock","mode":"0660","group":"mvmd"},"state_dir":"/var/lib/mvmd","driver":"memory","vmm_uid_range":{"first":1000,"count":65536}}`

// networkConfig is goodConfig with the network section.
func networkConfig(section string) string {
	return `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"network":` + section + `}`
}

func bootTimeoutConfig(d string) string {
	return `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"boot_timeout":"` + d + `"}`
}

func callTimeoutConfig(d string) string {
	return `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"call_timeout":"` + d + `"}`
}

func TestConfig_Parses(t *testing.T) {
	cfg, err := app.ParseConfig([]byte(goodConfig))
	require.NoError(t, err)
	assert.Equal(t, app.Config{
		Socket:      app.SocketConfig{Path: "/run/mvmd.sock", Mode: "0660", Group: "mvmd"},
		StateDir:    "/var/lib/mvmd",
		Driver:      "memory",
		VMMUIDRange: app.VMMUIDRangeConfig{First: 1000, Count: 65536},
	}, cfg)

	// Control for the supervision refusals: a well-formed section is accepted.
	withSupervision, err := app.ParseConfig([]byte(`{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"/usr/libexec/mvmd-supervisor","cgroup":"/sys/fs/cgroup/mvmd-supervisors"}}`))
	require.NoError(t, err)
	assert.Equal(t, &app.SupervisionConfig{Binary: "/usr/libexec/mvmd-supervisor", Cgroup: "/sys/fs/cgroup/mvmd-supervisors"}, withSupervision.Supervision)

	// A valid absolute vmm_cgroup is parsed into the struct.
	withVMMCgroup, err := app.ParseConfig([]byte(`{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"/usr/libexec/mvmd-supervisor","vmm_cgroup":"/sys/fs/cgroup/mvmd-vmm"}}`))
	require.NoError(t, err)
	assert.Equal(t, &app.SupervisionConfig{Binary: "/usr/libexec/mvmd-supervisor", VMMCgroup: "/sys/fs/cgroup/mvmd-vmm"}, withVMMCgroup.Supervision)

	// Control for the network refusals: a well-formed section is accepted.
	for _, section := range []string{
		`{"ipv4_prefix":"10.77.0.0/24","ipv4_gateway":"10.77.0.1","ipv6_prefix":"fd77::/64","resolvers":["10.77.0.2"]}`,
		`{"ipv4_prefix":"10.77.0.0/24","ipv4_gateway":"10.77.0.1"}`,
		`{"ipv6_prefix":"fd77::/64"}`,
	} {
		withNetwork, err := app.ParseConfig([]byte(networkConfig(section)))
		require.NoError(t, err, section)
		assert.NotNil(t, withNetwork.Network, section)
	}

	// Control for the boot timeout refusals: a duration within the bounds is
	// accepted, the longest one included.
	for _, d := range []string{"1ms", "90s", "1h"} {
		withTimeout, err := app.ParseConfig([]byte(bootTimeoutConfig(d)))
		require.NoError(t, err, d)
		assert.Equal(t, d, withTimeout.BootTimeout)
	}

	// Control for the call timeout refusals, the longest included.
	for _, d := range []string{"1ms", "5m", "6h"} {
		withTimeout, err := app.ParseConfig([]byte(callTimeoutConfig(d)))
		require.NoError(t, err, d)
		assert.Equal(t, d, withTimeout.CallTimeout)
	}

	path := filepath.Join(t.TempDir(), "mvmd.json")
	require.NoError(t, os.WriteFile(path, []byte(goodConfig), 0o600))
	loaded, err := app.LoadConfig(path)
	require.NoError(t, err)
	assert.Equal(t, cfg, loaded)
}

func TestConfig_Refuses(t *testing.T) {
	tests := []struct{ name, raw, reason string }{
		{"an option nothing reads", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","extra":1}`, "unknown field"},
		{"a relative socket path", `{"socket":{"path":"s","mode":"0660"},"state_dir":"/v","driver":"memory"}`, "socket.path"},
		{"a relative state directory", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"v","driver":"memory"}`, "state_dir"},
		{"a mode that is not octal", `{"socket":{"path":"/s","mode":"rw"},"state_dir":"/v","driver":"memory"}`, "socket.mode"},
		{"a mode above 0777", `{"socket":{"path":"/s","mode":"1777"},"state_dir":"/v","driver":"memory"}`, "socket.mode"},
		{"no mode", `{"socket":{"path":"/s"},"state_dir":"/v","driver":"memory"}`, "socket.mode"},
		{"an unknown driver", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"firecracker"}`, "driver"},
		{"a relative supervisor binary", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"mvmd-supervisor"}}`, "supervision.binary"},
		{"a relative supervisor directory", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"/b","dir":"d"}}`, "supervision.dir"},
		{"a relative supervisor cgroup", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"/b","cgroup":"c"}}`, "supervision.cgroup"},
		{"a relative vmm cgroup", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","vmm_uid_range":{"first":1000,"count":10},"supervision":{"binary":"/b","vmm_cgroup":"vmm"}}`, "supervision.vmm_cgroup"},
		{"a network that serves no family", networkConfig(`{}`), "serves neither"},
		{"an IPv4 prefix that does not parse", networkConfig(`{"ipv4_prefix":"10.77.0.0","ipv4_gateway":"10.77.0.1"}`), "network.ipv4_prefix"},
		{"an IPv4 prefix without a gateway", networkConfig(`{"ipv4_prefix":"10.77.0.0/24"}`), "network.ipv4_gateway"},
		{"a gateway without an IPv4 prefix", networkConfig(`{"ipv6_prefix":"fd77::/64","ipv4_gateway":"10.77.0.1"}`), "network.ipv4_gateway"},
		{"a gateway outside the prefix", networkConfig(`{"ipv4_prefix":"10.77.0.0/24","ipv4_gateway":"192.0.2.1"}`), "not inside"},
		{"an IPv6 prefix that does not parse", networkConfig(`{"ipv6_prefix":"fd77::"}`), "network.ipv6_prefix"},
		{"a resolver that does not parse", networkConfig(`{"ipv6_prefix":"fd77::/64","resolvers":["dns"]}`), "network.resolvers"},
		{"a boot timeout that is not a duration", bootTimeoutConfig("soon"), "boot_timeout"},
		{"a boot timeout without a unit", bootTimeoutConfig("90"), "boot_timeout"},
		{"a boot timeout of zero", bootTimeoutConfig("0s"), "boot_timeout"},
		{"a negative boot timeout", bootTimeoutConfig("-1s"), "boot_timeout"},
		{"a boot timeout above the maximum", bootTimeoutConfig("61m"), "boot_timeout"},
		{"a call timeout that is not a duration", callTimeoutConfig("soon"), "call_timeout"},
		{"a call timeout without a unit", callTimeoutConfig("90"), "call_timeout"},
		{"a call timeout of zero", callTimeoutConfig("0s"), "call_timeout"},
		{"a negative call timeout", callTimeoutConfig("-1s"), "call_timeout"},
		{"a call timeout above the maximum", callTimeoutConfig("7h"), "call_timeout"},
		{"trailing data", goodConfig + `{}`, "trailing"},
		{"not JSON", `socket: /s`, "parse"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := app.ParseConfig([]byte(tt.raw))
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
		})
	}
	_, err := app.LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	require.Error(t, err)
}
