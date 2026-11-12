package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// marker stands for a secret: the value of the env var and the content of the
// file the tests below give a Machine.
const marker = "MARKER-VALUE-4f9c1e"

// creator records the Spec it is asked to create, and answers Inspect with a
// Machine holding it, as the lifecycle does.
type creator struct {
	machine.Service
	spec machine.Spec
}

func (c *creator) Create(_ context.Context, _ machine.MachineID, _ machine.RequestID, spec machine.Spec) error {
	c.spec = spec
	return nil
}

func (c *creator) Inspect(context.Context, machine.MachineID) (machine.Machine, error) {
	return machine.Machine{ID: "m", Spec: c.spec}, nil
}

// CreateMachine carries command, args, env, user and files into the Spec, and
// the Machine it answers with reports the env names and the files' facts
// without a value or any content.
func TestCreateMachine_CarriesTheRunConfigurationAndReportsNoSecret(t *testing.T) {
	c := &creator{}
	server := api.New(c, nil, nil, machine.DriverInfo{}, nil, nil)

	resp, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{
		MachineId: "m",
		RequestId: "r",
		Command:   []string{"/bin/app"},
		Args:      []string{"--flag"},
		Env:       []*v1alpha2.EnvVar{{Name: "TOKEN", Value: marker}},
		User:      "1000:1000",
		Files:     []*v1alpha2.File{{Path: "/etc/app/key", Content: []byte(marker), Mode: 0o600, Uid: 1000, Gid: 1001}},
	})
	require.NoError(t, err)

	assert.Equal(t, []string{"/bin/app"}, c.spec.Command)
	assert.Equal(t, []string{"--flag"}, c.spec.Args)
	assert.Equal(t, []machine.EnvVar{{Name: "TOKEN", Value: marker}}, c.spec.Env)
	assert.Equal(t, "1000:1000", c.spec.User)
	assert.Equal(t, []machine.PlacedFile{{Path: "/etc/app/key", Content: []byte(marker), Mode: 0o600, UID: 1000, GID: 1001}}, c.spec.Files)

	assert.Equal(t, []string{"TOKEN"}, resp.Machine.EnvNames)
	require.Len(t, resp.Machine.Files, 1)
	assert.Equal(t, "/etc/app/key", resp.Machine.Files[0].Path)
	assert.Equal(t, uint32(0o600), resp.Machine.Files[0].Mode)
	assert.Equal(t, uint32(1000), resp.Machine.Files[0].Uid)
	assert.Equal(t, uint32(1001), resp.Machine.Files[0].Gid)
	assert.Equal(t, uint64(len(marker)), resp.Machine.Files[0].SizeBytes)

	inspected, err := server.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	wire, err := protojson.Marshal(inspected)
	require.NoError(t, err)
	assert.NotContains(t, string(wire), marker, "no value and no content in what Inspect answers")
	// Control: the Spec the lifecycle holds does carry it, so the check above
	// is not passing for want of a secret.
	assert.Equal(t, marker, c.spec.Env[0].Value)
}

// A refusal of the lifecycle reaches the caller as INVALID_ARGUMENT without
// the content that was refused.
func TestCreateMachine_ARefusedRunConfigurationEchoesNoSecret(t *testing.T) {
	refuse := &refuser{err: machine.NewError(machine.ErrInvalid, "files: path %q is under a reserved directory", "/proc/x")}
	server := api.New(refuse, nil, nil, machine.DriverInfo{}, nil, nil)

	_, err := server.CreateMachine(context.Background(), &v1alpha2.CreateMachineRequest{
		MachineId: "m",
		Files:     []*v1alpha2.File{{Path: "/proc/x", Content: []byte(marker)}},
		Env:       []*v1alpha2.EnvVar{{Name: "A", Value: marker}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "/proc/x", "it names the path")
	assert.NotContains(t, err.Error(), marker)
}

type refuser struct {
	machine.Service
	err error
}

func (r *refuser) Create(context.Context, machine.MachineID, machine.RequestID, machine.Spec) error {
	return r.err
}
