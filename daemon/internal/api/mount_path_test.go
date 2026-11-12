package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A mount path on a disk of Create Machine reaches the Spec and comes back on
// the Machine the call answers with.
func TestCreateMachine_CarriesTheMountPathOfADisk(t *testing.T) {
	c := &creator{}
	server := api.New(c, nil, nil, machine.DriverInfo{}, nil, nil)

	resp, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{
		MachineId: "m",
		RequestId: "r",
		Disks:     []*v1alpha2.DiskAttachment{{DiskId: "data", MountPath: "/data"}, {DiskId: "raw"}},
	})
	require.NoError(t, err)

	assert.Equal(t, []machine.DiskAttachment{{Disk: "data", MountPath: "/data"}, {Disk: "raw"}}, c.spec.Disks)
	require.Len(t, resp.Machine.Disks, 2)
	assert.Equal(t, "/data", resp.Machine.Disks[0].MountPath)
	assert.Empty(t, resp.Machine.Disks[1].MountPath)
}

// attacher records the attachment Attach disk is asked for.
type attacher struct {
	machine.Service
	got machine.DiskAttachment
}

func (a *attacher) AttachDisk(_ context.Context, _ machine.MachineID, att machine.DiskAttachment) error {
	a.got = att
	return nil
}

func (a *attacher) Inspect(context.Context, machine.MachineID) (machine.Machine, error) {
	return machine.Machine{ID: "m", Spec: machine.Spec{Disks: []machine.DiskAttachment{a.got}}}, nil
}

func TestAttachDisk_CarriesTheMountPath(t *testing.T) {
	a := &attacher{}

	resp, err := api.New(a, nil, nil, machine.DriverInfo{}, nil, nil).AttachDisk(context.Background(), &v1alpha2.AttachDiskRequest{
		MachineId: "m",
		Disk:      &v1alpha2.DiskAttachment{DiskId: "data", ReadOnly: true, MountPath: "/data"},
	})

	require.NoError(t, err)
	assert.Equal(t, machine.DiskAttachment{Disk: "data", ReadOnly: true, MountPath: "/data"}, a.got)
	assert.Equal(t, "/data", resp.Machine.Disks[0].MountPath)
}
