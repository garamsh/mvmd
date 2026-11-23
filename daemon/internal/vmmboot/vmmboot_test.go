package vmmboot_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmboot"
)

func valid() machine.BootSpec {
	return machine.BootSpec{
		RunDir:    "/run/m",
		Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20},
		Devices:   []machine.BlockDevice{{Disk: "root", Path: "/disks/root"}},
		Network:   machine.NetworkDevice{Tap: "tap0"},
	}
}

func TestValidate_RefusesWhatNoVMMRunsAndAcceptsTheNearestThatItRuns(t *testing.T) {
	require.NoError(t, vmmboot.Validate(valid()), "control: the description the cases are edits of")

	cases := []struct {
		name   string
		edit   func(*machine.BootSpec)
		reason string
		near   func(*machine.BootSpec)
	}{
		{"a tap name over the interface name", func(s *machine.BootSpec) { s.Network.Tap = strings.Repeat("t", vmmboot.MaxTapName+1) }, "tap name", func(s *machine.BootSpec) { s.Network.Tap = strings.Repeat("t", vmmboot.MaxTapName) }},
		{"a run directory that is not absolute", func(s *machine.BootSpec) { s.RunDir = "run/m" }, "run directory", nil},
		{"no vCPU", func(s *machine.BootSpec) { s.Resources.VCPUs = 0 }, "vCPUs", func(s *machine.BootSpec) { s.Resources.VCPUs = 1 }},
		{"more vCPUs than a Machine holds", func(s *machine.BootSpec) { s.Resources.VCPUs = vmmboot.MaxVCPUs + 1 }, "vCPUs", func(s *machine.BootSpec) { s.Resources.VCPUs = vmmboot.MaxVCPUs }},
		{"no memory", func(s *machine.BootSpec) { s.Resources.MemoryBytes = 0 }, "MiB", nil},
		{"memory that is not whole MiB", func(s *machine.BootSpec) { s.Resources.MemoryBytes = 256<<20 + 1 }, "MiB", nil},
		{"no block device in an image-form boot", func(s *machine.BootSpec) { s.Devices = nil }, "block device", nil},
		{"a block device path that is not absolute", func(s *machine.BootSpec) { s.Devices[0].Path = "disks/root" }, "not absolute", nil},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			spec := valid()
			tt.edit(&spec)
			assert.ErrorContains(t, vmmboot.Validate(spec), tt.reason)
			if tt.near != nil {
				spec = valid()
				tt.near(&spec)
				assert.NoError(t, vmmboot.Validate(spec), "control: the figure at the bound is accepted")
			}
		})
	}

	t.Run("an OS-form boot may hold no block device", func(t *testing.T) {
		spec := valid()
		spec.Devices = nil
		spec.Initrd = machine.KernelFile{Path: "/boot-sets/os/initrd", SHA256: strings.Repeat("a", 64)}
		assert.NoError(t, vmmboot.Validate(spec))
	})
}

func TestCheckCommandLine_RefusesALineAKernelWouldCut(t *testing.T) {
	require.NoError(t, vmmboot.CheckCommandLine(strings.Repeat("a", machine.MaxKernelCommandLine)), "control: the longest line the kernel takes")
	assert.ErrorContains(t, vmmboot.CheckCommandLine(strings.Repeat("a", machine.MaxKernelCommandLine+1)), "command line")
}

func TestInitrd_IsTheOSFormsAndOtherwiseTheConfiguredOne(t *testing.T) {
	configured := machine.File{Host: "/node/initrd", In: machine.InInitrd, ReadOnly: true, SHA256: "cfg"}
	assert.Equal(t, configured, vmmboot.Initrd(machine.BootSpec{}, "/node/initrd", "cfg"))

	spec := machine.BootSpec{Initrd: machine.KernelFile{Path: "/boot-sets/os/initrd", SHA256: "os"}}
	assert.Equal(t, machine.File{Host: "/boot-sets/os/initrd", In: machine.InInitrd, ReadOnly: true, SHA256: "os"}, vmmboot.Initrd(spec, "/node/initrd", "cfg"))
}

func TestResetParams_AddTheKeyboardControllerOnesOnAmd64Only(t *testing.T) {
	assert.Equal(t, "reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd", vmmboot.ResetParams("amd64"))
	assert.Equal(t, "reboot=k panic=1", vmmboot.ResetParams("arm64"))
}

func TestCPUCacheFiles_AreAarch64sAlone(t *testing.T) {
	assert.Equal(t, []string{"/sys/devices/system/cpu/cpu0/cache"}, vmmboot.CPUCacheFiles("arm64"))
	assert.Empty(t, vmmboot.CPUCacheFiles("amd64"))
}
