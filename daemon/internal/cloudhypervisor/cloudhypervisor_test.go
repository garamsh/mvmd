package cloudhypervisor_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var _ machine.Driver = (*cloudhypervisor.Driver)(nil)

func TestInfo_NamesCloudHypervisorAtItsVersionAndReportsOnlyTheConsole(t *testing.T) {
	info := newNode(t).driver.Info()
	assert.Equal(t, "cloud-hypervisor", info.Name)
	assert.Equal(t, "54.0", info.Version)
	assert.NotEmpty(t, info.CPUModel)
	assert.Equal(t, []machine.Capability{machine.CapabilityConsole}, info.Capabilities, "what the Driver implements and no more")
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
		{"a relative kernel path", func(c *cloudhypervisor.Config) { c.Kernel = "vmlinux" }, "absolute"},
		{"a relative initrd path", func(c *cloudhypervisor.Config) { c.Initrd = "initrd" }, "absolute"},
		{"a binary digest that is not hex", func(c *cloudhypervisor.Config) { c.BinarySHA256 = "sha256:abc" }, "digest"},
		{"an upper-case kernel digest", func(c *cloudhypervisor.Config) { c.KernelSHA256 = strings.ToUpper(c.KernelSHA256) }, "digest"},
		{"no run directories", func(c *cloudhypervisor.Config) { c.Dirs = nil }, "run directories"},
		{"a binary that is not the pinned one", func(c *cloudhypervisor.Config) { c.BinarySHA256 = strings.Repeat("0", 64) }, "binary"},
		{"a kernel that is not the pinned one", func(c *cloudhypervisor.Config) { c.KernelSHA256 = strings.Repeat("0", 64) }, "kernel"},
		{"a missing initrd", func(c *cloudhypervisor.Config) { c.Initrd = "/nonexistent/initrd" }, "initrd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := n.cfg
			tt.change(&cfg)
			_, err := cloudhypervisor.New(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNew_RefusesAMissingBinary(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.Remove(n.cfg.Binary))
	_, err := cloudhypervisor.New(n.cfg)
	require.Error(t, err)
}

// An operation whose capability is not reported refuses with ErrUnsupported
// (machine.Driver), and sends the VMM nothing.
func TestTheOperationsOfUnreportedCapabilitiesRefuseAsUnsupportedAndSendNothing(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())
	ctx := context.Background()
	spec := n.spec()

	_, snapErr := n.driver.Snapshot(ctx, "m1")
	_, descErr := n.driver.DescribeRestore(ctx, "m1", spec, nil)
	state, driveErr := n.driver.Drive(ctx, "m1", "root")
	tests := []struct {
		name string
		err  error
	}{
		{"Snapshot", snapErr},
		{"DescribeRestore", descErr},
		{"Restore", n.driver.Restore(ctx, "m1", spec)},
		{"AttachDisk", n.driver.AttachDisk(ctx, "m1", machine.BlockDevice{Disk: "d", Path: "/disks/d.img"})},
		{"DetachDisk", n.driver.DetachDisk(ctx, "m1", "root")},
		{"Drive", driveErr},
		{"Resize", n.driver.Resize(ctx, "m1", machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.ErrorIs(t, tt.err, machine.ErrUnsupported)
		})
	}
	assert.Equal(t, machine.DriveAbsent, state)
	assert.Len(t, f.seen(), before, "no request reached the VMM")

	for _, c := range n.driver.Info().Capabilities {
		assert.NotContains(t, []machine.Capability{machine.CapabilitySnapshot, machine.CapabilityDiskHotplug, machine.CapabilityResize}, c,
			"the Driver reports none of the capabilities it refuses")
	}
}
