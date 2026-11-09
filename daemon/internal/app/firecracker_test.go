package app_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

// firecrackerConfig is a config for the Firecracker Driver over files that
// carry the digests it names. The supervisor binary is only named: building the
// graph reads the host's Landlock ABI and starts nothing.
func firecrackerConfig(t *testing.T) app.Config {
	t.Helper()
	cfg := testConfig(t)
	dir := filepath.Dir(cfg.Socket.Path)
	file := func(name, content string) (path, sum string) {
		path = filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
		h := sha256.Sum256([]byte(content))
		return path, hex.EncodeToString(h[:])
	}
	f := &app.FirecrackerConfig{}
	f.Binary, f.BinarySHA256 = file("firecracker", "a binary")
	f.Kernel, f.KernelSHA256 = file("vmlinux", "a kernel")
	f.Initrd, _ = file("initrd.cpio", "an initrd")
	cfg.Driver = app.DriverFirecracker
	cfg.Firecracker = f
	cfg.Supervision = &app.SupervisionConfig{Binary: "/usr/libexec/mvmd-supervisor"}
	return cfg
}

func TestFirecracker_TheDriverAndTheChannelReplaceTheMemoryOnes(t *testing.T) {
	memory, err := app.New(testConfig(t))
	require.NoError(t, err)
	assert.NotNil(t, memory.Substitutes.Driver, "control: the memory config wires the memory Driver")
	assert.NotNil(t, memory.Substitutes.Guest, "control: and the memory Guest")

	a, err := app.New(firecrackerConfig(t))
	require.NoError(t, err)
	assert.Nil(t, a.Substitutes.Driver, "the memory Driver is not in the graph")
	assert.Nil(t, a.Substitutes.Guest, "the memory Guest is not in the graph")
	assert.Nil(t, a.Substitutes.Supervisors, "the Driver's run directories are the real supervisors'")
}

func TestFirecracker_ANodeWhoseInputsAreNotThePinnedOnesDoesNotStart(t *testing.T) {
	for _, file := range []string{"binary", "kernel"} {
		t.Run(file, func(t *testing.T) {
			cfg := firecrackerConfig(t)
			path := map[string]string{"binary": cfg.Firecracker.Binary, "kernel": cfg.Firecracker.Kernel}[file]
			pinned, err := os.ReadFile(path)
			require.NoError(t, err)

			require.NoError(t, os.WriteFile(path, []byte("another file"), 0o600))
			_, err = app.New(cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, "digest")

			// The control: the pinned file is accepted, and the refusal above
			// closed the database it had opened.
			require.NoError(t, os.WriteFile(path, pinned, 0o600))
			_, err = app.New(cfg)
			require.NoError(t, err)
		})
	}
}

const (
	digest64    = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	fcSection   = `,"firecracker":{"binary":"/opt/firecracker","binary_sha256":"` + digest64 + `","kernel":"/opt/vmlinux","kernel_sha256":"` + digest64 + `","initrd":"/opt/initrd.cpio"}`
	fcSupervise = `,"supervision":{"binary":"/usr/libexec/mvmd-supervisor"}`
)

func firecrackerRaw(driver, section, supervision string) string {
	return `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"` + driver + `","vmm_uid_range":{"first":1000,"count":10}` + section + supervision + `}`
}

func TestFirecracker_ConfigParses(t *testing.T) {
	cfg, err := app.ParseConfig([]byte(firecrackerRaw("firecracker", fcSection, fcSupervise)))
	require.NoError(t, err)
	assert.Equal(t, &app.FirecrackerConfig{
		Binary: "/opt/firecracker", BinarySHA256: digest64, Kernel: "/opt/vmlinux", KernelSHA256: digest64, Initrd: "/opt/initrd.cpio",
	}, cfg.Firecracker)
}

func TestFirecracker_ConfigRefuses(t *testing.T) {
	changed := func(old, with string) string {
		section := strings.Replace(fcSection, old, with, 1)
		require.NotEqual(t, fcSection, section, "the row changes the section")
		return firecrackerRaw("firecracker", section, fcSupervise)
	}
	tests := []struct{ name, raw, reason string }{
		{"the driver without its section", firecrackerRaw("firecracker", "", fcSupervise), "firecracker section"},
		{"the driver without supervision", firecrackerRaw("firecracker", fcSection, ""), "supervision section"},
		{"the section with the memory driver", firecrackerRaw("memory", fcSection, fcSupervise), "firecracker section is for"},
		{"a relative binary", changed(`"/opt/firecracker"`, `"firecracker"`), "firecracker binary"},
		{"a relative kernel", changed(`"/opt/vmlinux"`, `"vmlinux"`), "firecracker kernel"},
		{"a relative initrd", changed(`"/opt/initrd.cpio"`, `"initrd.cpio"`), "firecracker initrd"},
		{"a digest that is not hex", changed(digest64, "sha256:"+digest64), "digest"},
		{"an option the section does not read", firecrackerRaw("firecracker", `,"firecracker":{"extra":1}`, fcSupervise), "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := app.ParseConfig([]byte(tt.raw))
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
		})
	}
}
