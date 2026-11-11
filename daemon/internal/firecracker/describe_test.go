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
	assert.Equal(t, []string{"--api-sock", "/run/api.sock", "--enable-pci", "--log-path", "/run/firecracker.log"}, p.Args,
		"the VMM names its files by their paths in its root; Firecracker's own log goes to a file of its own, so vmm.log holds the console")
	assert.NotContains(t, strings.Join(p.Args, " "), n.run, "no argument names a host path")
	assert.NotContains(t, strings.Join(p.Args, " "), "seccomp", "the VMM's own filter stays on (driver.md)")
	assert.False(t, p.TapByDescriptor)
	assert.Equal(t, []string{"/dev/kvm", "/dev/net/tun"}, p.DeviceNodes)
	// The kernel and the initrd are read-only, then the Machine's devices as
	// given, each at its fixed path. The kernel and the initrd carry their pins.
	assert.Equal(t, []machine.File{
		{Host: n.cfg.Kernel, In: "/boot/kernel", ReadOnly: true, SHA256: n.cfg.KernelSHA256},
		{Host: n.cfg.Initrd, In: "/boot/initrd", ReadOnly: true, SHA256: n.cfg.InitrdSHA256},
		{Host: "/images/erofs", In: "/disk/0", ReadOnly: true},
		{Host: "/disks/root.img", In: "/disk/1"},
	}, p.BootFiles)
	assert.Equal(t, n.cfg.BinarySHA256, p.ExecutableSHA256)
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
		{"a tap name over the 15 bytes an interface name holds", func(s *machine.BootSpec) { s.Network.Tap = "tap-0123456789ab" }, "interface name"},
		{"a relative run directory", func(s *machine.BootSpec) { s.RunDir = "run" }, "absolute"},
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

// The digests are checked by the supervisor, on the file it presents to the
// VMM, and not here: a file this Driver hashed by its path could be replaced
// before the supervisor opened the path (ADR 0019). So the description carries
// the pins and Describe reads neither file.
func TestDescribe_StatesThePinsForTheSupervisorAndReadsNeitherFile(t *testing.T) {
	n := newNode(t)
	pinnedKernel, pinnedInitrd, pinnedBinary := n.cfg.KernelSHA256, n.cfg.InitrdSHA256, n.cfg.BinarySHA256
	for _, path := range []string{n.cfg.Kernel, n.cfg.Initrd, n.cfg.Binary} {
		require.NoError(t, os.WriteFile(path, []byte("another file"), 0o644))
	}

	p, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err, "a file that is no longer the pinned one is the supervisor's to refuse, as it opens it")

	assert.Equal(t, pinnedBinary, p.ExecutableSHA256, "the pin, not the digest of what the file holds now")
	assert.Equal(t, pinnedKernel, p.BootFiles[0].SHA256)
	assert.Equal(t, pinnedInitrd, p.BootFiles[1].SHA256, "the initrd is the guest's init, pinned as the kernel is")
	for _, f := range p.BootFiles[:2] {
		assert.NotEqual(t, sum([]byte("another file")), f.SHA256)
	}
}

func TestBootArgs_SkipTheKeyboardProbeOnX86OnlyAndNameTheEndMethodPerArchitecture(t *testing.T) {
	assert.Equal(t, "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd mvmd.end=reset", firecracker.BootArgs("amd64"))
	assert.Equal(t, "console=ttyS0 reboot=k panic=1 mvmd.end=poweroff", firecracker.BootArgs("arm64"))
}

func TestCPUReadFiles_AreNamedOnAarch64Only(t *testing.T) {
	assert.Empty(t, firecracker.CPUReadFiles("amd64"))
	assert.Equal(t, []string{
		"/sys/devices/system/cpu/cpu0/cache",
		"/sys/devices/system/cpu/cpu0/regs/identification/midr_el1",
	}, firecracker.CPUReadFiles("arm64"))
}

// Info reports the limit Describe enforces: the figure itself is accepted and
// the next one is refused.
func TestInfo_ReportsTheMostVCPUsDescribeAccepts(t *testing.T) {
	n := newNode(t)
	most := n.driver.Info().Limits.MaxVCPUs
	require.NotZero(t, most)

	spec := n.spec()
	spec.Resources.VCPUs = most
	_, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err, "control: the reported limit is accepted")

	spec.Resources.VCPUs = most + 1
	_, err = n.driver.Describe(context.Background(), "m1", spec)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "vCPUs")
}
