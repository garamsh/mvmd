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

// wantArgs is the command line of the spec of node.spec on x86_64.
var wantArgs = []string{
	"--api-socket", "path=/run/api.sock",
	"--log-file", "/run/cloud-hypervisor.log",
	"--kernel", "/boot/kernel",
	"--initramfs", "/boot/initrd",
	"--cmdline", "console=ttyS0",
	"--cpus", "boot=2",
	"--memory", "size=256M",
	"--serial", "tty",
	"--console", "off",
	"--vsock", "cid=3,socket=/run/vsock.sock",
	"--disk", "path=/disk/0,readonly=on,image_type=raw,id=disk0", "path=/disk/1,readonly=off,image_type=raw,id=disk1",
	"--net", "fd=[3],mac=02:00:00:00:00:01",
}

func TestDescribe_StatesTheProcessAndWhatItNeedsOpen(t *testing.T) {
	n := newNode(t)
	p, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err)

	assert.Equal(t, n.cfg.Binary, p.Executable)
	assert.Equal(t, wantArgs, cloudhypervisor.Args(n.spec(), "amd64"),
		"the VMM names its files by their paths in its root; its own log goes to a file of its own, so vmm.log holds the console; the serial port is the VMM's standard output and input")
	assert.Len(t, p.Args, len(wantArgs))
	assert.NotContains(t, strings.Join(p.Args, " "), n.run, "no argument names a host path")
	assert.NotContains(t, p.Args, "--seccomp", "the VMM's own filter stays on (driver.md)")
	assert.True(t, p.TapByDescriptor, "the tap is the VMM's descriptor 3")
	assert.Equal(t, []string{"/dev/kvm", "/dev/urandom"}, p.DeviceNodes, "kvm and the rng source; the tap is a descriptor, so no /dev/net/tun")
	// The kernel and the initrd are read-only, then the Machine's devices as
	// given, each at its fixed path. The kernel carries its pin.
	assert.Equal(t, []machine.File{
		{Host: n.cfg.Kernel, In: "/boot/kernel", ReadOnly: true, SHA256: n.cfg.KernelSHA256},
		{Host: n.cfg.Initrd, In: "/boot/initrd", ReadOnly: true},
		{Host: "/images/erofs", In: "/disk/0", ReadOnly: true},
		{Host: "/disks/root.img", In: "/disk/1"},
	}, p.BootFiles)
	assert.Equal(t, n.cfg.BinarySHA256, p.ExecutableSHA256)
	assert.Equal(t, machine.ProcessOverhead{MemoryBytes: 128 << 20, Tasks: 2 + 16 + 2, CPUs: 1}, p.Overhead)
}

func TestDescribe_ConfiguresEachPartOfTheVMFromTheSpec(t *testing.T) {
	n := newNode(t)
	spec := n.spec()
	spec.Resources = machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}
	spec.Devices = []machine.BlockDevice{{Path: "/images/erofs", ReadOnly: true}}
	spec.Network = machine.NetworkDevice{}
	p, err := n.driver.Describe(context.Background(), "m1", spec)
	require.NoError(t, err)

	joined := strings.Join(p.Args, " ")
	assert.Contains(t, joined, "--cpus boot=4 ")
	assert.Contains(t, joined, "--memory size=512M ")
	assert.Contains(t, joined, "--disk path=/disk/0,readonly=on,image_type=raw,id=disk0")
	assert.NotContains(t, joined, "/disk/1")
	assert.NotContains(t, joined, "--net", "a converter has no network device")
	assert.False(t, p.TapByDescriptor, "and no tap to pass")
	assert.Equal(t, uint32(4+16+1), p.Overhead.Tasks)
}

func TestDescribe_RefusesWhatTheVMMWould(t *testing.T) {
	tests := []struct {
		name   string
		change func(*machine.BootSpec)
		want   string
	}{
		{"no vCPU", func(s *machine.BootSpec) { s.Resources.VCPUs = 0 }, "vCPUs"},
		{"more vCPUs than a Machine has", func(s *machine.BootSpec) { s.Resources.VCPUs = 33 }, "vCPUs"},
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
	pinnedKernel, pinnedBinary := n.cfg.KernelSHA256, n.cfg.BinarySHA256
	for _, path := range []string{n.cfg.Kernel, n.cfg.Binary} {
		require.NoError(t, os.WriteFile(path, []byte("another file"), 0o644))
	}

	p, err := n.driver.Describe(context.Background(), "m1", n.spec())
	require.NoError(t, err, "a file that is no longer the pinned one is the supervisor's to refuse, as it opens it")

	assert.Equal(t, pinnedBinary, p.ExecutableSHA256, "the pin, not the digest of what the file holds now")
	assert.Equal(t, pinnedKernel, p.BootFiles[0].SHA256)
	assert.NotEqual(t, sum([]byte("another file")), p.BootFiles[0].SHA256)
}

func TestArgs_DifferOnAarch64ByTheSerialPortOnly(t *testing.T) {
	n := newNode(t)
	want := append([]string(nil), wantArgs...)
	want[9] = "console=ttyAMA0"
	assert.Equal(t, want, cloudhypervisor.Args(n.spec(), "arm64"))
}

func TestCmdline_NamesTheSerialPortOfTheArchitecture(t *testing.T) {
	assert.Equal(t, "console=ttyS0", cloudhypervisor.Cmdline("amd64"))
	assert.Equal(t, "console=ttyAMA0", cloudhypervisor.Cmdline("arm64"))
}

func TestCPUReadFiles_AreNamedOnAarch64Only(t *testing.T) {
	assert.Empty(t, cloudhypervisor.CPUReadFiles("amd64"))
	assert.Equal(t, []string{"/sys/devices/system/cpu/cpu0/cache"}, cloudhypervisor.CPUReadFiles("arm64"))
}
