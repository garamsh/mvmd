package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// starter records which start the API asked for.
type starter struct {
	machine.Service
	err     error
	plain   int
	updates []machine.RunUpdate
}

func (s *starter) Start(context.Context, machine.MachineID) error {
	s.plain++
	return s.err
}

func (s *starter) StartWith(_ context.Context, _ machine.MachineID, u machine.RunUpdate) error {
	s.updates = append(s.updates, u)
	return s.err
}

func (s *starter) Inspect(context.Context, machine.MachineID) (machine.Machine, error) {
	return machine.Machine{ID: "m"}, nil
}

// A present run_update reaches the lifecycle as env and files, and an absent
// one is the plain start.
func TestStartMachine_ARunUpdateReachesTheLifecycleAndAnAbsentOneDoesNot(t *testing.T) {
	s := &starter{}
	server := api.New(s, nil, nil, machine.DriverInfo{}, nil, nil)

	_, err := server.StartMachine(context.Background(), &v1alpha2.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, 1, s.plain, "no run_update: the plain start")
	assert.Empty(t, s.updates)

	_, err = server.StartMachine(context.Background(), &v1alpha2.StartMachineRequest{
		MachineId: "m",
		RunUpdate: &v1alpha2.RunUpdate{
			Env:   []*v1alpha2.EnvVar{{Name: "TOKEN", Value: marker}},
			Files: []*v1alpha2.File{{Path: "/etc/k", Content: []byte(marker), Mode: 0o600, Uid: 1, Gid: 2}},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, s.plain, "a run_update is not a plain start")
	require.Len(t, s.updates, 1)
	assert.Equal(t, []machine.EnvVar{{Name: "TOKEN", Value: marker}}, s.updates[0].Env)
	assert.Equal(t, []machine.PlacedFile{{Path: "/etc/k", Content: []byte(marker), Mode: 0o600, UID: 1, GID: 2}}, s.updates[0].Files)

	// An empty run_update is present: it reaches the lifecycle, to clear.
	_, err = server.StartMachine(context.Background(), &v1alpha2.StartMachineRequest{MachineId: "m", RunUpdate: &v1alpha2.RunUpdate{}})
	require.NoError(t, err)
	require.Len(t, s.updates, 2)
	assert.Empty(t, s.updates[1].Env)
	assert.Empty(t, s.updates[1].Files)
}

func TestStartMachine_ARefusedRunUpdateEchoesNoSecret(t *testing.T) {
	s := &starter{err: machine.NewError(machine.ErrInvalid, "files: path %q is under a reserved directory", "/proc/x")}
	server := api.New(s, nil, nil, machine.DriverInfo{}, nil, nil)

	_, err := server.StartMachine(context.Background(), &v1alpha2.StartMachineRequest{
		MachineId: "m",
		RunUpdate: &v1alpha2.RunUpdate{Files: []*v1alpha2.File{{Path: "/proc/x", Content: []byte(marker)}}},
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
	assert.Contains(t, err.Error(), "/proc/x")
	assert.NotContains(t, err.Error(), marker)
}

// CreateMachine carries working_dir into the Spec and Inspect reports it.
func TestCreateMachine_CarriesTheWorkingDirectoryAndInspectReportsIt(t *testing.T) {
	c := &creator{}
	server := api.New(c, nil, nil, machine.DriverInfo{}, nil, nil)

	resp, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{MachineId: "m", RequestId: "r", WorkingDir: "/srv/app"})
	require.NoError(t, err)
	assert.Equal(t, "/srv/app", c.spec.WorkingDir)
	assert.Equal(t, "/srv/app", resp.Machine.WorkingDir)

	inspected, err := server.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, "/srv/app", inspected.Machine.WorkingDir)

	// Control: none given, none carried.
	c2 := &creator{}
	_, err = api.New(c2, nil, nil, machine.DriverInfo{}, nil, nil).CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{MachineId: "m", RequestId: "r"})
	require.NoError(t, err)
	assert.Empty(t, c2.spec.WorkingDir)
}
