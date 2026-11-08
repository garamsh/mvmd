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
	withSupervision, err := app.ParseConfig([]byte(`{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","supervision":{"binary":"/usr/libexec/mvmd-supervisor","cgroup":"/sys/fs/cgroup/mvmd-supervisors"}}`))
	require.NoError(t, err)
	assert.Equal(t, &app.SupervisionConfig{Binary: "/usr/libexec/mvmd-supervisor", Cgroup: "/sys/fs/cgroup/mvmd-supervisors"}, withSupervision.Supervision)

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
		{"a relative supervisor binary", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","supervision":{"binary":"mvmd-supervisor"}}`, "supervision.binary"},
		{"a relative supervisor directory", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","supervision":{"binary":"/b","dir":"d"}}`, "supervision.dir"},
		{"a relative supervisor cgroup", `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"memory","supervision":{"binary":"/b","cgroup":"c"}}`, "supervision.cgroup"},
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
