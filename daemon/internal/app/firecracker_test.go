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
	cfg.StateDir = filepath.Join(verityDir(t), "state")
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
	cv := &app.ConversionConfig{
		Converter: "converter-1", VCPUs: 2, MemoryBytes: 512 << 20, OutputBytes: 1 << 30, NameSetBytes: 1 << 20, Limit: "10m",
	}
	cv.Kernel, cv.KernelSHA256 = file("converter-vmlinux", "a converter kernel")
	cv.Initrd, _ = file("converter-initrd.cpio", "a converter initrd")
	cfg.Driver = app.DriverFirecracker
	cfg.Firecracker = f
	cfg.Conversion = cv
	cfg.Supervision = &app.SupervisionConfig{Binary: "/usr/libexec/mvmd-supervisor"}
	return cfg
}

func TestFirecracker_TheDriverAndTheChannelReplaceTheMemoryOnes(t *testing.T) {
	memory, err := app.New(testConfig(t))
	require.NoError(t, err)
	assert.NotNil(t, memory.Substitutes.Driver, "control: the memory config wires the memory Driver")
	assert.NotNil(t, memory.Substitutes.Guest, "control: and the memory Guest")
	assert.NotNil(t, memory.Substitutes.Registry, "control: and the memory Registry")
	assert.NotNil(t, memory.Substitutes.Converter, "control: and the memory Converter")
	assert.NotNil(t, memory.Substitutes.Cache, "control: and the memory Image Store")
	assert.NotNil(t, memory.Substitutes.ConversionDriver, "control: and the memory conversion Driver")
	assert.NotNil(t, memory.Substitutes.ConversionSupervision, "control: and the memory conversion Supervision")

	a, err := app.New(firecrackerConfig(t))
	require.NoError(t, err)
	assert.Nil(t, a.Substitutes.Driver, "the memory Driver is not in the graph")
	assert.Nil(t, a.Substitutes.Guest, "the memory Guest is not in the graph")
	assert.Nil(t, a.Substitutes.Supervisors, "the Driver's run directories are the real supervisors'")
	assert.Nil(t, a.Substitutes.Registry, "the memory Registry is not in the graph")
	assert.Nil(t, a.Substitutes.Converter, "the memory Converter is not in the graph")
	assert.Nil(t, a.Substitutes.Cache, "the memory Image Store is not in the graph")
	assert.Nil(t, a.Substitutes.ConversionDriver, "the memory conversion Driver is not in the graph")
	assert.Nil(t, a.Substitutes.ConversionSupervision, "the memory conversion Supervision is not in the graph")
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
	cvSection   = `,"conversion":{"kernel":"/opt/converter-vmlinux","kernel_sha256":"` + digest64 + `","initrd":"/opt/converter-initrd.cpio","converter":"converter-1","vcpus":2,"memory_bytes":536870912,"output_bytes":1073741824,"name_set_bytes":1048576,"limit":"10m"}`
)

func firecrackerRaw(driver, section, supervision string) string {
	return `{"socket":{"path":"/s","mode":"0660"},"state_dir":"/v","driver":"` + driver + `","vmm_uid_range":{"first":1000,"count":10}` + section + supervision + `}`
}

func pullTimeoutRaw(d string) string {
	return strings.Replace(firecrackerRaw("firecracker", fcSection+cvSection, fcSupervise), `"driver"`, `"pull_timeout":"`+d+`","driver"`, 1)
}

func TestFirecracker_ConfigParses(t *testing.T) {
	cfg, err := app.ParseConfig([]byte(firecrackerRaw("firecracker", fcSection+cvSection, fcSupervise)))
	require.NoError(t, err)
	assert.Equal(t, &app.FirecrackerConfig{
		Binary: "/opt/firecracker", BinarySHA256: digest64, Kernel: "/opt/vmlinux", KernelSHA256: digest64, Initrd: "/opt/initrd.cpio",
	}, cfg.Firecracker)
	assert.Equal(t, &app.ConversionConfig{
		Kernel: "/opt/converter-vmlinux", KernelSHA256: digest64, Initrd: "/opt/converter-initrd.cpio", Converter: "converter-1",
		VCPUs: 2, MemoryBytes: 512 << 20, OutputBytes: 1 << 30, NameSetBytes: 1 << 20, Limit: "10m",
	}, cfg.Conversion)
	assert.Nil(t, cfg.Registry)

	// Control for the refusals of the registry section and the pull timeout.
	withRegistry := strings.Replace(firecrackerRaw("firecracker", fcSection+cvSection, fcSupervise), `"driver"`, `"registry":{"credentials_file":"/etc/mvmd/registry.json"},"pull_timeout":"45m","driver"`, 1)
	cfg, err = app.ParseConfig([]byte(withRegistry))
	require.NoError(t, err)
	assert.Equal(t, &app.RegistryConfig{CredentialsFile: "/etc/mvmd/registry.json"}, cfg.Registry)
	assert.Equal(t, "45m", cfg.PullTimeout)
}

func TestFirecracker_ConfigRefuses(t *testing.T) {
	changed := func(old, with string) string {
		section := strings.Replace(fcSection, old, with, 1)
		require.NotEqual(t, fcSection, section, "the row changes the section")
		return firecrackerRaw("firecracker", section+cvSection, fcSupervise)
	}
	conversion := func(old, with string) string {
		section := strings.Replace(cvSection, old, with, 1)
		require.NotEqual(t, cvSection, section, "the row changes the section")
		return firecrackerRaw("firecracker", fcSection+section, fcSupervise)
	}
	registry := func(driver, sections, section string) string {
		return strings.Replace(firecrackerRaw(driver, sections, fcSupervise), `"driver"`, `"registry":`+section+`,"driver"`, 1)
	}
	tests := []struct{ name, raw, reason string }{
		{"the driver without its section", firecrackerRaw("firecracker", "", fcSupervise), "firecracker section"},
		{"the driver without supervision", firecrackerRaw("firecracker", fcSection+cvSection, ""), "supervision section"},
		{"the driver without the conversion section", firecrackerRaw("firecracker", fcSection, fcSupervise), "conversion section"},
		{"the section with the memory driver", firecrackerRaw("memory", fcSection+cvSection, fcSupervise), "firecracker section is for"},
		{"the conversion section with the memory driver", firecrackerRaw("memory", cvSection, fcSupervise), "conversion section is for"},
		{"the registry section with the memory driver", registry("memory", "", `{"credentials_file":"/c"}`), "registry section is for"},
		{"a relative credentials file", registry("firecracker", fcSection+cvSection, `{"credentials_file":"c"}`), "registry.credentials_file"},
		{"no credentials file", registry("firecracker", fcSection+cvSection, `{}`), "registry.credentials_file"},
		{"a pull timeout that is not a duration", pullTimeoutRaw("soon"), "pull_timeout"},
		{"a pull timeout of zero", pullTimeoutRaw("0s"), "pull_timeout"},
		{"a pull timeout above the maximum", pullTimeoutRaw("7h"), "pull_timeout"},
		{"a relative converter kernel", conversion(`"/opt/converter-vmlinux"`, `"vmlinux"`), "conversion: firecracker kernel"},
		{"a relative converter initrd", conversion(`"/opt/converter-initrd.cpio"`, `"initrd"`), "conversion: firecracker initrd"},
		{"a converter kernel digest that is not hex", conversion(digest64, "sha256:"+digest64), "conversion: firecracker kernel digest"},
		{"no converter name", conversion(`"converter":"converter-1"`, `"converter":""`), "conversion.converter"},
		{"no converter vCPUs", conversion(`"vcpus":2`, `"vcpus":0`), "conversion.vcpus"},
		{"no converter memory", conversion(`"memory_bytes":536870912`, `"memory_bytes":0`), "conversion.memory_bytes"},
		{"no output size", conversion(`"output_bytes":1073741824`, `"output_bytes":0`), "conversion.output_bytes"},
		{"no name-set bound", conversion(`"name_set_bytes":1048576`, `"name_set_bytes":0`), "conversion.name_set_bytes"},
		{"a conversion limit that is not a duration", conversion(`"limit":"10m"`, `"limit":"soon"`), "conversion.limit"},
		{"a conversion limit of zero", conversion(`"limit":"10m"`, `"limit":"0s"`), "conversion.limit"},
		{"a conversion limit above the maximum", conversion(`"limit":"10m"`, `"limit":"61m"`), "conversion.limit"},
		{"an option the conversion section does not read", conversion(`"limit":"10m"`, `"limit":"10m","extra":1`), "unknown field"},
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
