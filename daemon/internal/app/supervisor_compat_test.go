package app_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
	"github.com/garamsh/mvmd/daemon/internal/supervisor"
	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// reporting is a supervisor that answers Inspect with the API version it names.
type reporting struct {
	pb.UnimplementedSupervisorServiceServer
	machine, version string
}

func (r reporting) Inspect(context.Context, *pb.InspectRequest) (*pb.InspectResponse, error) {
	return &pb.InspectResponse{MachineId: r.machine, ApiVersion: r.version}, nil
}

// serveSupervisor makes the state directory of the Machine and serves a supervisor on its socket.
func serveSupervisor(t *testing.T, dir, machine, version string) {
	t.Helper()
	state := filepath.Join(dir, machine)
	require.NoError(t, os.MkdirAll(state, 0o700))
	l, err := unixsock.Listen(statedir.SupervisorSocket(state))
	require.NoError(t, err)
	g := grpc.NewServer()
	pb.RegisterSupervisorServiceServer(g, reporting{machine: machine, version: version})
	go func() { _ = g.Serve(l) }()
	t.Cleanup(g.Stop)
}

func TestSpeaks_TheVersionOfItsOwnReleaseAndOneThatReportedNone(t *testing.T) {
	assert.True(t, supervision.Speaks(supervisor.APIVersion))
	assert.True(t, supervision.Speaks(""), "a release that did not report a version spoke v1alpha1")
	assert.False(t, supervision.Speaks("v1alpha2"))
	assert.False(t, supervision.Speaks("v2"))
}

// A start finds, before it changes anything, a supervisor that reports an API this mvmd does
// not speak, and names its Machine and the version; one that reports none, and one that reports
// this release's, are spoken to.
func TestCheckCompatible_NamesTheFirstSupervisorItDoesNotSpeakTo(t *testing.T) {
	dir, err := os.MkdirTemp("", "sv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	a, err := supervision.New(supervision.Config{Binary: "/absent-supervisor", Dir: dir})
	require.NoError(t, err)
	serveSupervisor(t, dir, "a-old", "")
	serveSupervisor(t, dir, "b-current", supervisor.APIVersion)
	require.NoError(t, a.CheckCompatible(context.Background()), "control: the supervisors it speaks to are accepted")

	serveSupervisor(t, dir, "d-future", "v1alpha2")
	serveSupervisor(t, dir, "c-future", "v2")
	err = a.CheckCompatible(context.Background())

	var incompatible *supervision.IncompatibleError
	require.ErrorAs(t, err, &incompatible)
	assert.Equal(t, "c-future", string(incompatible.Machine), "the first by Machine identifier")
	assert.Equal(t, "v2", incompatible.APIVersion)
	assert.ErrorContains(t, err, "nothing was changed")
}
