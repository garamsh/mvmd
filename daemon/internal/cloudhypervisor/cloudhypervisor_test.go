package cloudhypervisor_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ machine.Driver = (*cloudhypervisor.Driver)(nil)

func TestInfo_NamesCloudHypervisorAtItsVersionAndReportsTheConsoleSnapshotAndDiskHotplug(t *testing.T) {
	info := newNode(t).driver.Info()
	assert.Equal(t, "cloud-hypervisor", info.Name)
	assert.Equal(t, "54.0", info.Version)
	assert.NotEmpty(t, info.CPUModel)
	assert.Subset(t, info.Capabilities, []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot, machine.CapabilityDiskHotplug})
}

// Resize is x86_64's in v54.0, so only x86_64 reports it; the Driver's
// architecture is named, so both cases run on any host.
func TestInfo_ReportsResizeOnX8664Only(t *testing.T) {
	for arch, reported := range map[string]bool{"amd64": true, "arm64": false} {
		n := newNode(t)
		cloudhypervisor.SetGOARCH(n.driver, arch)
		caps := n.driver.Info().Capabilities
		assert.Equal(t, reported, slices.Contains(caps, machine.CapabilityCPUResize), arch)
		assert.Equal(t, reported, slices.Contains(caps, machine.CapabilityMemoryResize), arch)
		assert.Subset(t, caps, []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot, machine.CapabilityDiskHotplug}, "%s: the rest is unchanged", arch)
	}
}

func TestNew_RefusesAConfigurationThatNamesNoPinnedInput(t *testing.T) {
	n := newNode(t)
	_, err := cloudhypervisor.New(n.cfg)
	require.NoError(t, err, "control: the node's own configuration is accepted")

	tests := []struct {
		name   string
		change func(*cloudhypervisor.Config)
		want   string
	}{
		{"a relative binary path", func(c *cloudhypervisor.Config) { c.Binary = "cloud-hypervisor" }, "absolute"},
		{"a relative kernel path", func(c *cloudhypervisor.Config) {
			c.Kernels = []machine.KernelFile{{Path: "vmlinux", SHA256: c.Kernels[0].SHA256}}
		}, "absolute"},
		{"a relative path on a later kernel", func(c *cloudhypervisor.Config) {
			c.Kernels = []machine.KernelFile{c.Kernels[0], {Path: "vmlinux", SHA256: c.Kernels[1].SHA256}}
		}, "absolute"},
		{"no kernel", func(c *cloudhypervisor.Config) { c.Kernels = nil }, "kernel"},
		{"a relative initrd path", func(c *cloudhypervisor.Config) { c.Initrd = "initrd" }, "absolute"},
		{"a binary digest that is not hex", func(c *cloudhypervisor.Config) { c.BinarySHA256 = "sha256:abc" }, "digest"},
		{"an upper-case kernel digest", func(c *cloudhypervisor.Config) {
			c.Kernels = []machine.KernelFile{{Path: c.Kernels[0].Path, SHA256: strings.ToUpper(c.Kernels[0].SHA256)}}
		}, "digest"},
		{"an initrd digest that is not hex", func(c *cloudhypervisor.Config) { c.InitrdSHA256 = "sha256:abc" }, "digest"},
		{"no run directories", func(c *cloudhypervisor.Config) { c.Dirs = nil }, "run directories"},
		{"a binary that is not the pinned one", func(c *cloudhypervisor.Config) { c.BinarySHA256 = strings.Repeat("0", 64) }, "binary"},
		{"a kernel that is not the pinned one", func(c *cloudhypervisor.Config) {
			c.Kernels = []machine.KernelFile{{Path: c.Kernels[0].Path, SHA256: strings.Repeat("0", 64)}, c.Kernels[1]}
		}, "kernel"},
		{"a later kernel that is not the pinned one", func(c *cloudhypervisor.Config) {
			c.Kernels = []machine.KernelFile{c.Kernels[0], {Path: c.Kernels[1].Path, SHA256: strings.Repeat("0", 64)}}
		}, "kernel"},
		{"an initrd that is not the pinned one", func(c *cloudhypervisor.Config) { c.InitrdSHA256 = strings.Repeat("0", 64) }, "initrd"},
		{"a missing initrd", func(c *cloudhypervisor.Config) { c.Initrd = "/nonexistent/initrd" }, "initrd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := n.cfg
			tt.change(&cfg)
			_, err := cloudhypervisor.New(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
			assert.Contains(t, err.Error(), "cloud-hypervisor", "the error names the VMM")
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
	assert.Contains(t, err.Error(), "cloud-hypervisor binary")
}

func TestNew_RefusesAMissingBinary(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.Remove(n.cfg.Binary))
	_, err := cloudhypervisor.New(n.cfg)
	require.Error(t, err)
}

// Where the architecture has no resize, Resize is refused with ErrUnsupported
// (machine.Driver), sends the VMM nothing, and is not reported. The control is
// an operation the Driver does implement, which does reach the VMM and is
// reported.
func TestResizeRefusesAsUnsupportedAndSendsNothingWhereTheArchitectureHasNone(t *testing.T) {
	n := newNode(t)
	cloudhypervisor.SetGOARCH(n.driver, "arm64")
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	err := n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20})
	require.ErrorIs(t, err, machine.ErrUnsupported)
	assert.Len(t, f.seen(), before, "no request reached the VMM")
	assert.NotContains(t, n.driver.Info().Capabilities, machine.CapabilityCPUResize, "the Driver does not report what it refuses")
	assert.NotContains(t, n.driver.Info().Capabilities, machine.CapabilityMemoryResize, "the Driver does not report what it refuses")

	_, err = n.driver.Drive(context.Background(), "m1", "root")
	require.NoError(t, err, "control: an implemented operation is not refused")
	assert.Greater(t, len(f.seen()), before, "and it reaches the VMM")
	assert.Contains(t, n.driver.Info().Capabilities, machine.CapabilityDiskHotplug)

	cloudhypervisor.SetGOARCH(n.driver, "amd64")
	mid := len(f.seen())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_ = n.driver.Resize(ctx, "m1", machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}) // the fake VMM never reports the target
	assert.Greater(t, len(f.seen()), mid, "control: the same Driver on x86_64 reaches the VMM, so the architecture is what refused")
}
