package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

const (
	success      = v1alpha1.OutcomeKind_OUTCOME_KIND_SUCCESS
	rejected     = v1alpha1.OutcomeKind_OUTCOME_KIND_REJECTED
	notFound     = v1alpha1.OutcomeKind_OUTCOME_KIND_NOT_FOUND
	conflict     = v1alpha1.OutcomeKind_OUTCOME_KIND_CONFLICT
	precondition = v1alpha1.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED
	unsupported  = v1alpha1.OutcomeKind_OUTCOME_KIND_UNSUPPORTED
)

func create(id string) *v1alpha1.CreateMachineRequest {
	return &v1alpha1.CreateMachineRequest{
		MachineId:     id,
		RequestId:     "req-" + id,
		ImageDigest:   digest,
		Resources:     &v1alpha1.Resources{CpuCount: 2, MemoryBytes: 1 << 30},
		RootDiskBytes: 1 << 30,
		NetworkPolicy: &v1alpha1.NetworkPolicy{Egress: []*v1alpha1.NetworkRule{{
			Prefix:   "10.0.0.0/8",
			Protocol: v1alpha1.Protocol_PROTOCOL_TCP,
			Ports:    []*v1alpha1.PortRange{{First: 443, Last: 443}},
		}}},
	}
}

func renew(t *testing.T, n *node, number uint64) *v1alpha1.DeadlineStatus {
	t.Helper()
	resp, err := n.client.RenewDeadline(context.Background(), &v1alpha1.RenewDeadlineRequest{
		Length: durationpb.New(time.Hour), RenewalNumber: number,
	})
	require.NoError(t, err)
	return resp.GetStatus()
}

// Every implemented RPC, driven through lib/runtimeapi's client over the
// socket of the wired daemon.
func TestAPI_DrivesTheWholeLifecycle(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	n.app.Substitutes.Disks.Provide("data")

	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha1.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.Equal(t, []string{"mvmd.runtime.v1alpha1"}, info.GetApiVersions())
	require.Len(t, info.GetDrivers(), 1)
	assert.Equal(t, "memory", info.GetDrivers()[0].GetDriver().GetName())
	assert.ElementsMatch(t, []v1alpha1.Capability{
		v1alpha1.Capability_CAPABILITY_DISK_HOTPLUG, v1alpha1.Capability_CAPABILITY_CPU_RESIZE, v1alpha1.Capability_CAPABILITY_MEMORY_RESIZE,
	}, info.GetDrivers()[0].GetCapabilities())

	// The deadline is not armed: a start is refused with its outcome.
	st, err := n.client.InspectDeadline(ctx, &v1alpha1.InspectDeadlineRequest{})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED, st.GetStatus().GetPhase())

	created, err := n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_CREATED, created.GetMachine().GetState())
	assert.Equal(t, "m", created.GetMachine().GetId())
	assert.Equal(t, uint32(2), created.GetMachine().GetResources().GetCpuCount())
	assert.Equal(t, uint64(1<<30), created.GetMachine().GetRootDiskBytes())
	assert.NotEmpty(t, created.GetMachine().GetAddresses())
	assert.Equal(t, "memory", created.GetMachine().GetDriver().GetName())
	rule := created.GetMachine().GetNetworkPolicy().GetEgress()
	require.Len(t, rule, 1)
	assert.Equal(t, "10.0.0.0/8", rule[0].GetPrefix())
	assert.Equal(t, v1alpha1.Protocol_PROTOCOL_TCP, rule[0].GetProtocol())
	assert.Equal(t, uint32(443), rule[0].GetPorts()[0].GetFirst())

	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "m"})
	requireKind(t, err, precondition)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))

	armed := renew(t, n, 1)
	assert.Equal(t, v1alpha1.DeadlinePhase_DEADLINE_PHASE_ARMED, armed.GetPhase())
	assert.Equal(t, time.Hour, armed.GetLength().AsDuration())
	assert.Equal(t, uint64(1), armed.GetLastRenewalNumber())
	assert.LessOrEqual(t, armed.GetRemaining().AsDuration(), time.Hour)
	again := renew(t, n, 1)
	assert.Equal(t, uint64(1), again.GetLastRenewalNumber(), "a number not greater changes nothing")
	inspected, err := n.client.InspectDeadline(ctx, &v1alpha1.InspectDeadlineRequest{})
	require.NoError(t, err)
	assert.Equal(t, uint64(1), inspected.GetStatus().GetLastRenewalNumber())

	started, err := n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, started.GetMachine().GetState())
	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "m"})
	require.NoError(t, err, "a repeat in the target state succeeds")

	paused, err := n.client.PauseMachine(ctx, &v1alpha1.PauseMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_PAUSED, paused.GetMachine().GetState())
	resumed, err := n.client.ResumeMachine(ctx, &v1alpha1.ResumeMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, resumed.GetMachine().GetState())

	attached, err := n.client.AttachDisk(ctx, &v1alpha1.AttachDiskRequest{MachineId: "m", Disk: &v1alpha1.DiskAttachment{DiskId: "data", ReadOnly: true}})
	require.NoError(t, err)
	require.Len(t, attached.GetMachine().GetDisks(), 1)
	assert.True(t, attached.GetMachine().GetDisks()[0].GetReadOnly())
	detached, err := n.client.DetachDisk(ctx, &v1alpha1.DetachDiskRequest{MachineId: "m", DiskId: "data"})
	require.NoError(t, err)
	assert.Empty(t, detached.GetMachine().GetDisks())

	resized, err := n.client.SetResources(ctx, &v1alpha1.SetResourcesRequest{MachineId: "m", Target: &v1alpha1.Resources{CpuCount: 4, MemoryBytes: 2 << 30}})
	require.NoError(t, err)
	assert.Equal(t, uint32(4), resized.GetResources().GetCpuCount())
	assert.Equal(t, uint64(2<<30), resized.GetResources().GetMemoryBytes())

	one, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, uint32(4), one.GetMachine().GetResources().GetCpuCount())
	_, err = n.client.CreateMachine(ctx, create("a"))
	require.NoError(t, err)
	list, err := n.client.ListMachines(ctx, &v1alpha1.ListMachinesRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetMachines(), 2)
	assert.Equal(t, "a", list.GetMachines()[0].GetId())

	_, err = n.client.DeleteMachine(ctx, &v1alpha1.DeleteMachineRequest{MachineId: "m"})
	requireKind(t, err, precondition) // Running

	stopped, err := n.client.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(time.Second)})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, stopped.GetMachine().GetState())
	_, err = n.client.DeleteMachine(ctx, &v1alpha1.DeleteMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	_, err = n.client.DeleteMachine(ctx, &v1alpha1.DeleteMachineRequest{MachineId: "m"})
	require.NoError(t, err, "absent: success")
	_, err = n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "m"})
	requireKind(t, err, notFound)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

// Outcome kinds as status codes and details, for the creating call.
func TestAPI_CreateOutcomes(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)

	_, err := n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, create("m"))
	require.NoError(t, err, "the same request ID and parameters return the original outcome")

	other := create("m")
	other.RequestId = "another"
	_, err = n.client.CreateMachine(ctx, other)
	requireKind(t, err, conflict)
	assert.Equal(t, codes.AlreadyExists, status.Code(err))

	different := create("m")
	different.Resources.CpuCount = 8
	_, err = n.client.CreateMachine(ctx, different)
	requireKind(t, err, conflict)

	missing := create("x")
	missing.ImageDigest = "sha256:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	_, err = n.client.CreateMachine(ctx, missing)
	requireKind(t, err, notFound)
	assert.Equal(t, codes.NotFound, status.Code(err))

	noDisk := create("y")
	noDisk.Disks = []*v1alpha1.DiskAttachment{{DiskId: "absent"}}
	_, err = n.client.CreateMachine(ctx, noDisk)
	requireKind(t, err, notFound)
}

// The server validates for itself: what the client would refuse is sent raw.
func TestAPI_ServerValidatesEveryRequest(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	raw := n.raw(t)

	bad := create("m")
	bad.ImageDigest = "latest"
	_, err := raw.CreateMachine(ctx, bad)
	requireKind(t, err, rejected)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))

	noRequest := create("m")
	noRequest.RequestId = ""
	_, err = raw.CreateMachine(ctx, noRequest)
	requireKind(t, err, rejected)

	badPolicy := create("m")
	badPolicy.NetworkPolicy.Egress[0].Ports = []*v1alpha1.PortRange{{First: 9, Last: 8}}
	_, err = raw.CreateMachine(ctx, badPolicy)
	requireKind(t, err, rejected)

	noResources := create("m")
	noResources.Resources = nil
	_, err = raw.CreateMachine(ctx, noResources)
	requireKind(t, err, rejected)

	_, err = n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "m"})
	requireKind(t, err, notFound) // none of the above created it

	calls := map[string]func() error{
		"start":   func() error { _, err := raw.StartMachine(ctx, &v1alpha1.StartMachineRequest{}); return err },
		"stop":    func() error { _, err := raw.StopMachine(ctx, &v1alpha1.StopMachineRequest{}); return err },
		"pause":   func() error { _, err := raw.PauseMachine(ctx, &v1alpha1.PauseMachineRequest{}); return err },
		"resume":  func() error { _, err := raw.ResumeMachine(ctx, &v1alpha1.ResumeMachineRequest{}); return err },
		"delete":  func() error { _, err := raw.DeleteMachine(ctx, &v1alpha1.DeleteMachineRequest{}); return err },
		"inspect": func() error { _, err := raw.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{}); return err },
		"attach": func() error {
			_, err := raw.AttachDisk(ctx, &v1alpha1.AttachDiskRequest{Disk: &v1alpha1.DiskAttachment{DiskId: "d"}})
			return err
		},
		"attach, no disk": func() error {
			_, err := raw.AttachDisk(ctx, &v1alpha1.AttachDiskRequest{MachineId: "m"})
			return err
		},
		"detach": func() error { _, err := raw.DetachDisk(ctx, &v1alpha1.DetachDiskRequest{DiskId: "d"}); return err },
		"detach, no disk": func() error {
			_, err := raw.DetachDisk(ctx, &v1alpha1.DetachDiskRequest{MachineId: "m"})
			return err
		},
		"set resources, no target": func() error {
			_, err := raw.SetResources(ctx, &v1alpha1.SetResourcesRequest{MachineId: "m"})
			return err
		},
		"stop, negative grace": func() error {
			_, err := raw.StopMachine(ctx, &v1alpha1.StopMachineRequest{MachineId: "m", GracePeriod: durationpb.New(-time.Second)})
			return err
		},
		"renew, no length": func() error {
			_, err := raw.RenewDeadline(ctx, &v1alpha1.RenewDeadlineRequest{RenewalNumber: 1})
			return err
		},
		"renew, zero length": func() error {
			_, err := raw.RenewDeadline(ctx, &v1alpha1.RenewDeadlineRequest{Length: durationpb.New(0), RenewalNumber: 1})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) { requireKind(t, call(), rejected) })
	}
	st, err := n.client.InspectDeadline(ctx, &v1alpha1.InspectDeadlineRequest{})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED, st.GetStatus().GetPhase(), "a rejected renewal arms nothing")
}

// A call the inside does not offer yet is UNSUPPORTED with a reason.
func TestAPI_UnofferedCallsAreUnsupportedWithAReason(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	c := n.client
	pull := &v1alpha1.PullImageRequest{Image: &v1alpha1.ImageRef{Repository: "registry.example/app", Digest: digest}}

	unary := map[string]func() error{
		"PullImage": func() error { _, err := c.PullImage(ctx, pull); return err },
	}
	streams := map[string]func() error{
		"Console": func() error {
			s, err := c.Console(ctx)
			require.NoError(t, err)
			_ = s.Send(&v1alpha1.ConsoleRequest{MachineId: "m"})
			_, err = s.Recv()
			return err
		},
	}
	for _, calls := range []map[string]func() error{unary, streams} {
		for name, call := range calls {
			t.Run(name, func(t *testing.T) {
				err := call()
				requireKind(t, err, unsupported)
				assert.Equal(t, codes.Unimplemented, status.Code(err))
				assert.Contains(t, status.Convert(err).Message(), name)
			})
		}
	}
	assert.Len(t, unary, 1)
	assert.Len(t, streams, 1)
}
