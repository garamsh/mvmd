package firecracker_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ machine.Driver = (*firecracker.Driver)(nil)

func TestInfo_NamesFirecrackerAtItsVersionAndReportsTheConsoleSnapshotAndDiskHotplug(t *testing.T) {
	info := newNode(t).driver.Info()
	assert.Equal(t, "firecracker", info.Name)
	assert.Equal(t, "1.17.0", info.Version)
	assert.NotEmpty(t, info.CPUModel)
	assert.Equal(t, []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot, machine.CapabilityDiskHotplug}, info.Capabilities, "resize is not one: v1.17.0 has none this Driver reaches")
}

func TestNew_RefusesAConfigurationThatNamesNoPinnedInput(t *testing.T) {
	n := newNode(t)
	_, err := firecracker.New(n.cfg)
	require.NoError(t, err, "control: the node's own configuration is accepted")

	tests := []struct {
		name   string
		change func(*firecracker.Config)
		want   string
	}{
		{"a relative binary path", func(c *firecracker.Config) { c.Binary = "firecracker" }, "absolute"},
		{"a relative kernel path", func(c *firecracker.Config) {
			c.Kernels = []machine.KernelFile{{Path: "vmlinux", SHA256: c.Kernels[0].SHA256}}
		}, "absolute"},
		{"a relative path on a later kernel", func(c *firecracker.Config) {
			c.Kernels = []machine.KernelFile{c.Kernels[0], {Path: "vmlinux", SHA256: c.Kernels[1].SHA256}}
		}, "absolute"},
		{"no kernel", func(c *firecracker.Config) { c.Kernels = nil }, "kernel"},
		{"a relative initrd path", func(c *firecracker.Config) { c.Initrd = "initrd" }, "absolute"},
		{"a binary digest that is not hex", func(c *firecracker.Config) { c.BinarySHA256 = "sha256:abc" }, "digest"},
		{"an upper-case kernel digest", func(c *firecracker.Config) {
			c.Kernels = []machine.KernelFile{{Path: c.Kernels[0].Path, SHA256: strings.ToUpper(c.Kernels[0].SHA256)}}
		}, "digest"},
		{"an initrd digest that is not hex", func(c *firecracker.Config) { c.InitrdSHA256 = "sha256:abc" }, "digest"},
		{"no run directories", func(c *firecracker.Config) { c.Dirs = nil }, "run directories"},
		{"a binary that is not the pinned one", func(c *firecracker.Config) { c.BinarySHA256 = strings.Repeat("0", 64) }, "binary"},
		{"a kernel that is not the pinned one", func(c *firecracker.Config) {
			c.Kernels = []machine.KernelFile{{Path: c.Kernels[0].Path, SHA256: strings.Repeat("0", 64)}, c.Kernels[1]}
		}, "kernel"},
		{"a later kernel that is not the pinned one", func(c *firecracker.Config) {
			c.Kernels = []machine.KernelFile{c.Kernels[0], {Path: c.Kernels[1].Path, SHA256: strings.Repeat("0", 64)}}
		}, "kernel"},
		{"an initrd that is not the pinned one", func(c *firecracker.Config) { c.InitrdSHA256 = strings.Repeat("0", 64) }, "initrd"},
		{"a missing initrd", func(c *firecracker.Config) { c.Initrd = "/nonexistent/initrd" }, "initrd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := n.cfg
			tt.change(&cfg)
			_, err := firecracker.New(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "firecracker", "the error names the VMM")
		})
	}
}

// What the console, its log and the guest's ports are is guestaccess's, and
// its tests hold them. This holds that the Driver is wired to it: the run
// directories its configuration names.
func TestConsoleLog_IsTheLogOfTheRunDirectoryTheConfigurationNames(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.WriteFile(filepath.Join(n.run, "vmm.log"), []byte("boot\n"), 0o600))
	r, err := n.driver.ConsoleLog(context.Background(), "m1")
	require.NoError(t, err)
	defer r.Close()
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "boot\n", string(b))
}

func TestConfigValidate_RefusesAConfigurationThatNamesNoPinnedInputAndNamesTheVMM(t *testing.T) {
	cfg := newNode(t).cfg
	require.NoError(t, cfg.Validate(), "control: the node's own configuration is accepted")

	cfg.Binary = "relative"
	err := cfg.Validate()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "firecracker binary")
}

func TestNew_RefusesAMissingBinary(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.Remove(n.cfg.Binary))
	_, err := firecracker.New(n.cfg)
	require.Error(t, err)
}
