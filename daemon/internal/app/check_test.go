package app_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/diskfile"
)

// pinned writes a file and returns its path and SHA-256.
func pinned(t *testing.T, dir, name, content string) (string, string) {
	t.Helper()
	path := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o755))
	sum := sha256.Sum256([]byte(content))
	return path, hex.EncodeToString(sum[:])
}

// checkedConfig names real files with their digests and a mkfs that succeeds: what a node
// with a Firecracker section, a conversion, boot sets and a disk section is checked on.
func checkedConfig(t *testing.T) app.Config {
	t.Helper()
	dir := t.TempDir()
	bin, binSum := pinned(t, dir, "firecracker", "binary")
	kernel, kernelSum := pinned(t, dir, "vmlinux", "kernel")
	initrd, initrdSum := pinned(t, dir, "initrd", "initrd")
	osKernel, osKernelSum := pinned(t, dir, "os-vmlinux", "os kernel")
	osInitrd, osInitrdSum := pinned(t, dir, "os-initrd", "os initrd")
	convKernel, convKernelSum := pinned(t, dir, "conv-vmlinux", "converter kernel")
	convInitrd, convInitrdSum := pinned(t, dir, "conv-initrd", "converter initrd")
	mkfs, _ := pinned(t, dir, "mkfs.ext4", "#!/bin/sh\nexit 0\n")
	return app.Config{
		StateDir: filepath.Join(dir, "state"),
		Driver:   app.DriverFirecracker,
		Firecracker: &app.VMMConfig{
			Binary: bin, BinarySHA256: binSum,
			Kernels:       map[string]app.KernelConfig{"default": {Path: kernel, SHA256: kernelSum}},
			DefaultKernel: "default",
			Initrd:        initrd, InitrdSHA256: initrdSum,
			BootSets: map[string]app.BootSetConfig{"os": {Kernel: app.KernelConfig{Path: osKernel, SHA256: osKernelSum}, Initrd: app.KernelConfig{Path: osInitrd, SHA256: osInitrdSum}}},
		},
		Conversion: &app.ConversionConfig{Kernel: convKernel, KernelSHA256: convKernelSum, Initrd: convInitrd, InitrdSHA256: convInitrdSum},
		Disk:       &app.DiskConfig{MKFS: mkfs},
	}
}

func TestCheckHost_CountsEachPinnedFileAndRefusesTheFirstThatIsNotTheOnePinned(t *testing.T) {
	cfg := checkedConfig(t)

	covered, err := cfg.CheckHost()
	require.NoError(t, err)
	assert.Equal(t, "checked 7 pinned file(s), mkfs.ext4", covered, "the binary, the guest initrd, one kernel, a boot set's two files and the converter's two, and mkfs")
	assert.NoDirExists(t, cfg.StateDir, "the check made no state directory")

	cfg.Firecracker.BootSets = map[string]app.BootSetConfig{"os": {
		Kernel: cfg.Firecracker.BootSets["os"].Kernel,
		Initrd: app.KernelConfig{Path: cfg.Firecracker.BootSets["os"].Initrd.Path, SHA256: cfg.Firecracker.Kernels["default"].SHA256},
	}}
	_, err = cfg.CheckHost()
	require.Error(t, err)
	assert.ErrorContains(t, err, `the initrd of boot set "os"`, "the refusal names the file")
}

func TestCheckHost_RefusesAMkfsThatDoesNotRun(t *testing.T) {
	cfg := checkedConfig(t)
	cfg.Disk.MKFS = filepath.Join(t.TempDir(), "absent")

	_, err := cfg.CheckHost()

	assert.ErrorIs(t, err, diskfile.ErrBadMKFS)
}

// mvmd -check loads the configuration, checks it and the host, says what it covered, and exits
// without a database, a socket or a state directory.
func TestMain_CheckEndsWithoutStartingAnything(t *testing.T) {
	cfg := testConfig(t)
	raw := []byte(`{"socket":{"path":"` + cfg.Socket.Path + `","mode":"0600"},"state_dir":"` + cfg.StateDir + `","driver":"memory","vmm_uid_range":{"first":1000,"count":65536}}`)
	file := filepath.Join(t.TempDir(), "mvmd.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))

	var out bytes.Buffer
	code := app.Main([]string{"-check", "-config", file}, &out)

	assert.Zero(t, code, out.String())
	assert.Contains(t, out.String(), "the configuration is valid; checked 0 pinned file(s)")
	assert.NoDirExists(t, cfg.StateDir)
	assert.NoFileExists(t, cfg.Socket.Path)

	require.NoError(t, os.WriteFile(file, []byte(`{"state_dir":"relative"}`), 0o600))
	out.Reset()
	assert.Equal(t, 1, app.Main([]string{"-check", "-config", file}, &out), "control: a configuration that is refused is exit code 1")
	assert.Contains(t, out.String(), "mvmd:")
}
