package firecracker_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestInfo_NamesFirecrackerAtItsVersionAndReportsTheConsoleAndSnapshot(t *testing.T) {
	info := newNode(t).driver.Info()
	assert.Equal(t, "firecracker", info.Name)
	assert.Equal(t, "1.17.0", info.Version)
	assert.NotEmpty(t, info.CPUModel)
	assert.Equal(t, []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot}, info.Capabilities, "hot-plug and resize are not implemented")
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
		{"a relative kernel path", func(c *firecracker.Config) { c.Kernel = "vmlinux" }, "absolute"},
		{"a relative initrd path", func(c *firecracker.Config) { c.Initrd = "initrd" }, "absolute"},
		{"a binary digest that is not hex", func(c *firecracker.Config) { c.BinarySHA256 = "sha256:abc" }, "digest"},
		{"an upper-case kernel digest", func(c *firecracker.Config) { c.KernelSHA256 = strings.ToUpper(c.KernelSHA256) }, "digest"},
		{"no run directories", func(c *firecracker.Config) { c.Dirs = nil }, "run directories"},
		{"a binary that is not the pinned one", func(c *firecracker.Config) { c.BinarySHA256 = strings.Repeat("0", 64) }, "binary"},
		{"a kernel that is not the pinned one", func(c *firecracker.Config) { c.KernelSHA256 = strings.Repeat("0", 64) }, "kernel"},
		{"a missing initrd", func(c *firecracker.Config) { c.Initrd = "/nonexistent/initrd" }, "initrd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := n.cfg
			tt.change(&cfg)
			_, err := firecracker.New(cfg)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestNew_RefusesAMissingBinary(t *testing.T) {
	n := newNode(t)
	require.NoError(t, os.Remove(n.cfg.Binary))
	_, err := firecracker.New(n.cfg)
	require.Error(t, err)
}

func TestDriver_RefusesTheOperationsItDoesNotYetImplement(t *testing.T) {
	d := newNode(t).driver
	ctx := context.Background()
	for name, err := range map[string]error{
		"attach": d.AttachDisk(ctx, "m1", machine.BlockDevice{}),
		"detach": d.DetachDisk(ctx, "m1", "d"),
		"resize": d.Resize(ctx, "m1", machine.Resources{}),
	} {
		assert.ErrorIs(t, err, machine.ErrUnsupported, name)
	}
}
