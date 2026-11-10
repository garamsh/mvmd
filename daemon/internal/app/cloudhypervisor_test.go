package app_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const chSection = `,"cloud_hypervisor":{"binary":"/opt/cloud-hypervisor","binary_sha256":"` + digest64 + `","kernel":"/opt/vmlinux","kernel_sha256":"` + digest64 + `","initrd":"/opt/initrd.cpio","initrd_sha256":"` + digest64 + `"}`

// cloudHypervisorConfig is firecrackerConfig with the Cloud Hypervisor Driver
// selected over the same files: the files of the firecracker section are the
// cloud_hypervisor section's, and the node names no Firecracker binary.
func cloudHypervisorConfig(t *testing.T) app.Config {
	t.Helper()
	cfg := firecrackerConfig(t)
	cfg.Driver = app.DriverCloudHypervisor
	cfg.CloudHypervisor = (*app.CloudHypervisorConfig)(cfg.Firecracker)
	cfg.Firecracker = nil
	return cfg
}

func TestCloudHypervisor_TheDriverAndTheChannelReplaceTheMemoryOnes(t *testing.T) {
	memory, err := app.New(testConfig(t))
	require.NoError(t, err)
	assert.NotNil(t, memory.Substitutes.Driver, "control: the memory config wires the memory Driver")
	assert.NotNil(t, memory.Substitutes.Registry, "control: and the memory Registry")
	assert.NotNil(t, memory.Substitutes.ConversionDriver, "control: and the memory conversion Driver")

	a, err := app.New(cloudHypervisorConfig(t))
	require.NoError(t, err)
	assert.Nil(t, a.Substitutes.Driver, "the memory Driver is not in the graph")
	assert.Nil(t, a.Substitutes.Guest, "the memory Guest is not in the graph")
	assert.Nil(t, a.Substitutes.Supervisors, "the Driver's run directories are the real supervisors'")
	assert.Nil(t, a.Substitutes.Registry, "the memory Registry is not in the graph")
	assert.Nil(t, a.Substitutes.Converter, "the memory Converter is not in the graph")
	assert.Nil(t, a.Substitutes.Disks, "the memory Disk is not in the graph")
	assert.Nil(t, a.Substitutes.Snapshots, "the memory Snapshot Store is not in the graph")
	assert.Nil(t, a.Substitutes.Cache, "the memory Image Store is not in the graph")
	assert.Nil(t, a.Substitutes.ConversionDriver, "the memory conversion Driver is not in the graph")
	assert.Nil(t, a.Substitutes.ConversionSupervision, "the memory conversion Supervision is not in the graph")
}

// The selected Driver serves the node's Machines and boots its converter VMs:
// a cloud-hypervisor node has no Firecracker in its graph.
func TestDriverSelection_BothTheMachinesAndTheConvertersRunOnTheSelectedDriver(t *testing.T) {
	tests := []struct {
		driver string
		config func(*testing.T) app.Config
	}{
		{app.DriverFirecracker, firecrackerConfig},
		{app.DriverCloudHypervisor, cloudHypervisorConfig},
	}
	for _, tt := range tests {
		t.Run(tt.driver, func(t *testing.T) {
			cfg := tt.config(t)
			n := start(t, cfg)

			info, err := n.client.GetRuntimeInfo(context.Background(), &v1alpha1.GetRuntimeInfoRequest{})
			require.NoError(t, err)
			require.Len(t, info.GetDrivers(), 1)
			assert.Equal(t, tt.driver, info.GetDrivers()[0].GetDriver().GetName(), "the Machines' Driver")
			assert.Equal(t, tt.driver, app.ConversionDriver(n.app).Info().Name, "the converters' Driver")
		})
	}
}

func TestCloudHypervisor_ANodeWhoseInputsAreNotThePinnedOnesDoesNotStart(t *testing.T) {
	for _, file := range []string{"binary", "kernel", "initrd", "converter initrd"} {
		t.Run(file, func(t *testing.T) {
			cfg := cloudHypervisorConfig(t)
			path := map[string]string{
				"binary": cfg.CloudHypervisor.Binary, "kernel": cfg.CloudHypervisor.Kernel,
				"initrd": cfg.CloudHypervisor.Initrd, "converter initrd": cfg.Conversion.Initrd,
			}[file]
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

func cloudHypervisorRaw(driver, section, supervision string) string {
	return firecrackerRaw(driver, section, supervision)
}

func TestCloudHypervisor_ConfigParses(t *testing.T) {
	cfg, err := app.ParseConfig([]byte(cloudHypervisorRaw("cloud-hypervisor", chSection+cvSection+dkSection, fcSupervise)))
	require.NoError(t, err, "no firecracker section is needed: the converter boots on the selected Driver")
	assert.Equal(t, app.DriverCloudHypervisor, cfg.Driver)
	assert.Equal(t, &app.CloudHypervisorConfig{
		Binary: "/opt/cloud-hypervisor", BinarySHA256: digest64, Kernel: "/opt/vmlinux", KernelSHA256: digest64, Initrd: "/opt/initrd.cpio", InitrdSHA256: digest64,
	}, cfg.CloudHypervisor)
	assert.Nil(t, cfg.Firecracker)
	assert.Equal(t, &app.DiskConfig{MKFS: "/sbin/mkfs.ext4"}, cfg.Disk)
}

func TestCloudHypervisor_ConfigRefuses(t *testing.T) {
	changed := func(old, with string) string {
		section := strings.Replace(chSection, old, with, 1)
		require.NotEqual(t, chSection, section, "the row changes the section")
		return cloudHypervisorRaw("cloud-hypervisor", section+cvSection+dkSection, fcSupervise)
	}
	conversion := func(old, with string) string {
		section := strings.Replace(cvSection, old, with, 1)
		require.NotEqual(t, cvSection, section, "the row changes the section")
		return cloudHypervisorRaw("cloud-hypervisor", chSection+section+dkSection, fcSupervise)
	}
	tests := []struct{ name, raw, reason string }{
		{"the driver without its section", cloudHypervisorRaw("cloud-hypervisor", cvSection+dkSection, fcSupervise), "cloud_hypervisor section"},
		{"the driver with the Firecracker section instead", cloudHypervisorRaw("cloud-hypervisor", fcSection+cvSection+dkSection, fcSupervise), "cloud_hypervisor section"},
		{"the driver with both sections", cloudHypervisorRaw("cloud-hypervisor", chSection+fcSection+cvSection+dkSection, fcSupervise), "firecracker section is for"},
		{"the Firecracker driver with the Cloud Hypervisor section", cloudHypervisorRaw("firecracker", fcSection+chSection+cvSection+dkSection, fcSupervise), "cloud_hypervisor section is for"},
		{"the section with the memory driver", cloudHypervisorRaw("memory", chSection+cvSection, fcSupervise), "cloud_hypervisor section is for"},
		{"the driver without supervision", cloudHypervisorRaw("cloud-hypervisor", chSection+cvSection+dkSection, ""), "supervision section"},
		{"the driver without the conversion section", cloudHypervisorRaw("cloud-hypervisor", chSection+dkSection, fcSupervise), "conversion section"},
		{"the driver without the disk section", cloudHypervisorRaw("cloud-hypervisor", chSection+cvSection, fcSupervise), "disk section"},
		{"a relative binary", changed(`"/opt/cloud-hypervisor"`, `"cloud-hypervisor"`), "cloud-hypervisor binary"},
		{"a relative kernel", changed(`"/opt/vmlinux"`, `"vmlinux"`), "cloud-hypervisor kernel"},
		{"a relative initrd", changed(`"/opt/initrd.cpio"`, `"initrd.cpio"`), "cloud-hypervisor initrd"},
		{"no initrd digest", changed(`,"initrd_sha256":"`+digest64+`"`, ``), "cloud-hypervisor initrd digest"},
		{"a binary digest that is not hex", changed(`"binary_sha256":"`+digest64, `"binary_sha256":"sha256:`+digest64), "cloud-hypervisor binary digest"},
		{"a relative converter kernel", conversion(`"/opt/converter-vmlinux"`, `"vmlinux"`), "conversion: cloud-hypervisor kernel"},
		{"no converter initrd digest", conversion(`"initrd_sha256":"`+digest64+`",`, ``), "conversion: cloud-hypervisor initrd digest"},
		{"an option the section does not read", cloudHypervisorRaw("cloud-hypervisor", `,"cloud_hypervisor":{"extra":1}`, fcSupervise), "unknown field"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := app.ParseConfig([]byte(tt.raw))
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
		})
	}
}
