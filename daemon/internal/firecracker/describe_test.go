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

func TestDescribe_StatesTheProcessAndWhatItNeedsOpen(t *testing.T) {
	n := newNode(t)
	p, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	assert.Equal(t, n.cfg.Binary, p.Executable)
	assert.Equal(t, []string{"--api-sock", n.apiSocket(), "--enable-pci"}, p.Args)
	assert.NotContains(t, strings.Join(p.Args, " "), "seccomp", "the VMM's own filter stays on (driver.md)")
	assert.False(t, p.TapByDescriptor)
	assert.Equal(t, []string{"/dev/kvm", "/dev/net/tun"}, p.DeviceNodes)
	// The kernel and the initrd are read-only, then the Machine's devices as given.
	assert.Equal(t, []machine.BlockDevice{
		{Path: n.cfg.Kernel, ReadOnly: true},
		{Path: n.cfg.Initrd, ReadOnly: true},
		{Disk: "image", Path: "/images/erofs", ReadOnly: true},
		{Disk: "root", Path: "/disks/root.img"},
	}, p.BootFiles)
	assert.Equal(t, machine.ProcessOverhead{MemoryBytes: 128 << 20, Tasks: 2 + 8, CPUs: 1}, p.Overhead)
}

func TestDescribe_ATapIsOpenedOnlyForAMachineWithANetworkDevice(t *testing.T) {
	n := newNode(t)
	spec := n.spec()
	p, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	assert.Contains(t, p.DeviceNodes, "/dev/net/tun", "control: a Machine has its tap")

	spec.Network = machine.NetworkDevice{}
	p, err = n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	assert.Equal(t, []string{"/dev/kvm"}, p.DeviceNodes, "a converter has no network device")
}

func TestDescribe_TasksFollowTheVCPUs(t *testing.T) {
	n := newNode(t)
	spec := n.spec()
	spec.Resources.VCPUs = 16
	p, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)
	assert.Equal(t, uint32(16+8), p.Overhead.Tasks)
}

func TestDescribe_RefusesWhatTheVMMWould(t *testing.T) {
	tests := []struct {
		name   string
		change func(*machine.BootSpec)
		want   string
	}{
		{"no vCPU", func(s *machine.BootSpec) { s.Resources.VCPUs = 0 }, "vCPUs"},
		{"more vCPUs than v1.17.0 takes", func(s *machine.BootSpec) { s.Resources.VCPUs = 33 }, "vCPUs"},
		{"memory that is not whole MiB", func(s *machine.BootSpec) { s.Resources.MemoryBytes = 256<<20 + 1 }, "MiB"},
		{"no memory", func(s *machine.BootSpec) { s.Resources.MemoryBytes = 0 }, "MiB"},
		{"no block device", func(s *machine.BootSpec) { s.Devices = nil }, "block device"},
		{"a relative device path", func(s *machine.BootSpec) { s.Devices[0].Path = "erofs" }, "not absolute"},
		{"a relative run directory", func(s *machine.BootSpec) { s.RunDir = "run" }, "absolute"},
		{"a run directory too long for a socket", func(s *machine.BootSpec) { s.RunDir = "/" + strings.Repeat("d", 100) }, "Unix socket"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			_, err := n.driver.Describe(context.Background(), "m1", n.spec())
			require.NoError(t, err, "control: the unchanged description is accepted")

			spec := n.spec()
			tt.change(&spec)
			_, err = n.driver.Describe(context.Background(), "m1", spec)
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestDescribe_ChecksTheDigestsBeforeEveryBoot(t *testing.T) {
	for _, file := range []string{"binary", "kernel"} {
		t.Run(file, func(t *testing.T) {
			n := newNode(t)
			_, err := n.driver.Describe(context.Background(), "m1", n.spec())
			require.NoError(t, err, "control: the pinned file is accepted")

			path := map[string]string{"binary": n.cfg.Binary, "kernel": n.cfg.Kernel}[file]
			require.NoError(t, os.WriteFile(path, []byte("another file"), 0o644))
			_, err = n.driver.Describe(context.Background(), "m1", n.spec())
			require.Error(t, err)
			assert.Contains(t, err.Error(), "digest")
			assert.Contains(t, err.Error(), file)
		})
	}
}

func TestBootArgs_SkipTheKeyboardProbeOnX86Only(t *testing.T) {
	assert.Equal(t, "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd", firecracker.BootArgs("amd64"))
	assert.Equal(t, "console=ttyS0 reboot=k panic=1", firecracker.BootArgs("arm64"))
}

func TestCPUReadFiles_AreNamedOnAarch64Only(t *testing.T) {
	assert.Empty(t, firecracker.CPUReadFiles("amd64"))
	assert.Equal(t, []string{
		"/sys/devices/system/cpu/cpu0/cache",
		"/sys/devices/system/cpu/cpu0/regs/identification/midr_el1",
	}, firecracker.CPUReadFiles("arm64"))
}
