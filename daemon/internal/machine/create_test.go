package machine_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Row "Create Machine", column Needs.
func TestCreate_Needs(t *testing.T) {
	invalid := func(edit func(*machine.Spec)) func(*testing.T, *env, *machine.Spec) {
		return func(_ *testing.T, _ *env, s *machine.Spec) { edit(s) }
	}
	tests := []struct {
		name  string
		setup func(*testing.T, *env, *machine.Spec)
		kind  error
	}{
		{"image absent from the node", invalid(func(s *machine.Spec) { s.Image = "sha256:other" }), machine.ErrNotFound},
		{"no vCPU", invalid(func(s *machine.Spec) { s.Resources.VCPUs = 0 }), machine.ErrInvalid},
		{"no memory", invalid(func(s *machine.Spec) { s.Resources.MemoryBytes = 0 }), machine.ErrInvalid},
		{"no root disk size", invalid(func(s *machine.Spec) { s.RootDiskBytes = 0 }), machine.ErrInvalid},
		{"disk the node does not hold", invalid(func(s *machine.Spec) {
			s.Disks = []machine.DiskAttachment{{Disk: "missing"}}
		}), machine.ErrNotFound},
		{"disk listed twice", func(_ *testing.T, e *env, s *machine.Spec) {
			e.disks.Provide("d")
			s.Disks = []machine.DiskAttachment{{Disk: "d"}, {Disk: "d", ReadOnly: true}}
		}, machine.ErrInvalid},
		{"disk another Machine holds", func(t *testing.T, e *env, s *machine.Spec) {
			e.disks.Provide("d")
			e.created(t, "other")
			require.NoError(t, e.svc.AttachDisk(context.Background(), "other", machine.DiskAttachment{Disk: "d"}))
			s.Disks = []machine.DiskAttachment{{Disk: "d", ReadOnly: true}}
		}, machine.ErrPreconditionFailed},
		{"policy with an unparseable prefix", invalid(func(s *machine.Spec) {
			s.Policy.Egress = []machine.NetworkRule{{}}
		}), machine.ErrInvalid},
		{"ICMP with an IPv6 prefix", invalid(func(s *machine.Spec) {
			s.Policy.Ingress = []machine.NetworkRule{{Prefix: netip.MustParsePrefix("fd00::/8"), Protocol: machine.ProtocolICMP}}
		}), machine.ErrInvalid},
		{"ICMPv6 with an IPv4 prefix", invalid(func(s *machine.Spec) {
			s.Policy.Egress = []machine.NetworkRule{{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Protocol: machine.ProtocolICMPv6}}
		}), machine.ErrInvalid},
		{"ports on an any-protocol rule", invalid(func(s *machine.Spec) {
			s.Policy.Egress = []machine.NetworkRule{{Prefix: netip.MustParsePrefix("10.0.0.0/8"), Ports: []machine.PortRange{{First: 1, Last: 2}}}}
		}), machine.ErrInvalid},
		{"port zero", invalid(func(s *machine.Spec) {
			s.Policy.Egress[0].Ports = []machine.PortRange{{First: 0, Last: 5}}
		}), machine.ErrInvalid},
		{"range with first above last", invalid(func(s *machine.Spec) {
			s.Policy.Egress[0].Ports = []machine.PortRange{{First: 9, Last: 8}}
		}), machine.ErrInvalid},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			spec := baseSpec()
			tt.setup(t, e, &spec)
			roots := e.disks.Calls("create-root")
			err := e.svc.Create(context.Background(), "m", "r", spec)
			requireKind(t, err, tt.kind)
			e.requireNoMachine(t, "m")
			assert.Equal(t, roots, e.disks.Calls("create-root"), "nothing was made")

			// Control: the same call with a spec that meets every need
			// succeeds, so the refusal above was the need and nothing else.
			require.NoError(t, e.svc.Create(context.Background(), "m", "r", baseSpec()))
		})
	}

	t.Run("an empty policy is valid and closed", func(t *testing.T) {
		e := newEnv()
		spec := baseSpec()
		spec.Policy = machine.NetworkPolicy{}
		require.NoError(t, e.svc.Create(context.Background(), "m", "r", spec))
	})
}

// Row "Create Machine", column On success.
func TestCreate_OnSuccess(t *testing.T) {
	e := newEnv()
	e.disks.Provide("data")
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{{Disk: "data", ReadOnly: true}}
	require.NoError(t, e.svc.Create(context.Background(), "m", "r", spec))

	m := e.requireState(t, "m", machine.StateCreated)
	assert.Equal(t, spec, m.Spec)
	assert.Equal(t, machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: []machine.Capability{machine.CapabilityConsole}}, m.Driver)
	assert.NotEmpty(t, m.Network.MAC)
	assert.NotEmpty(t, m.Network.Addresses)
	size, held := e.disks.Holds(m.RootDisk)
	assert.True(t, held)
	assert.Equal(t, spec.RootDiskBytes, size, "the root disk has the requested size")
	e.requireStored(t, "m", machine.StateCreated)

	// Creation only configures: no VMM process, no boot, no policy applied.
	_, running := e.sup.Running("m")
	assert.False(t, running)
	assert.Zero(t, e.driver.Calls("boot"))
	_, applied := e.net.Applied("m")
	assert.False(t, applied)
}

// Row "Create Machine", column Repeat.
func TestCreate_Repeat(t *testing.T) {
	t.Run("same request ID and parameters return the original outcome", func(t *testing.T) {
		e := newEnv()
		ctx := context.Background()
		e.created(t, "m")
		require.NoError(t, e.svc.Start(ctx, "m"))

		require.NoError(t, e.svc.Create(ctx, "m", request("m"), baseSpec()))
		assert.Equal(t, 1, e.disks.Calls("create-root"), "no second root disk")
		e.requireState(t, "m", machine.StateRunning)
	})

	t.Run("another request ID is a conflict and changes nothing", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		err := e.svc.Create(context.Background(), "m", "other", baseSpec())
		requireKind(t, err, machine.ErrConflict)
		assert.Equal(t, 1, e.disks.Calls("create-root"))
		assert.Equal(t, 1, e.net.Calls("allocate"))
		e.requireState(t, "m", machine.StateCreated)
	})

	edits := []struct {
		name string
		edit func(*env, *machine.Spec)
	}{
		{"image", func(e *env, s *machine.Spec) { e.images.Provide("sha256:b"); s.Image = "sha256:b" }},
		{"vCPUs", func(_ *env, s *machine.Spec) { s.Resources.VCPUs++ }},
		{"memory", func(_ *env, s *machine.Spec) { s.Resources.MemoryBytes++ }},
		{"root disk size", func(_ *env, s *machine.Spec) { s.RootDiskBytes++ }},
		{"disks", func(e *env, s *machine.Spec) {
			e.disks.Provide("d")
			s.Disks = []machine.DiskAttachment{{Disk: "d"}}
		}},
		{"network policy", func(_ *env, s *machine.Spec) { s.Policy = machine.NetworkPolicy{} }},
	}
	for _, tt := range edits {
		t.Run("other "+tt.name+" is a conflict and changes nothing", func(t *testing.T) {
			e := newEnv()
			e.created(t, "m")
			before, err := e.svc.Inspect(context.Background(), "m")
			require.NoError(t, err)

			spec := baseSpec()
			tt.edit(e, &spec)
			require.NotEqual(t, baseSpec(), spec, "the edit changed the spec")
			err = e.svc.Create(context.Background(), "m", request("m"), spec)
			requireKind(t, err, machine.ErrConflict)

			after, err := e.svc.Inspect(context.Background(), "m")
			require.NoError(t, err)
			assert.Equal(t, before, after)
			assert.Equal(t, 1, e.disks.Calls("create-root"))
		})
	}

	t.Run("a failed create repeats as the same failure and does not run again", func(t *testing.T) {
		e := newEnv()
		ctx := context.Background()
		e.disks.FailAt("create-root", errBoom)
		first := e.svc.Create(ctx, "m", "r", baseSpec())
		requireKind(t, first, machine.ErrFailed)

		e.disks.Heal("create-root")
		again := e.svc.Create(ctx, "m", "r", baseSpec())
		requireKind(t, again, machine.ErrFailed)
		assert.Equal(t, first, again, "the original outcome, reason included")
		assert.Equal(t, 1, e.disks.Calls("create-root"))
		e.requireNoMachine(t, "m")

		// With no Machine, the identifier is free for a new request.
		require.NoError(t, e.svc.Create(ctx, "m", "r2", baseSpec()))
	})
}

// Row "Create Machine", column State after failure.
func TestCreate_StateAfterFailure(t *testing.T) {
	steps := []struct {
		name string
		fail func(*env)
	}{
		{"network allocation", func(e *env) { e.net.FailAt("allocate", errBoom) }},
		{"root disk creation", func(e *env) { e.disks.FailAt("create-root", errBoom) }},
		{"image lookup", func(e *env) { e.images.FailAt("locate", errBoom) }},
		{"recording the Machine", func(e *env) { e.store.FailAt("save", errBoom) }},
	}
	for _, tt := range steps {
		t.Run("fails at "+tt.name+" and no Machine exists", func(t *testing.T) {
			e := newEnv()
			tt.fail(e)
			err := e.svc.Create(context.Background(), "m", "r", baseSpec())
			requireKind(t, err, machine.ErrFailed)
			e.requireNoMachine(t, "m")
			assert.Zero(t, e.sup.Calls("start"))

			// Control: the same call succeeds once nothing is made to fail.
			e.net.Heal("allocate")
			e.disks.Heal("create-root")
			e.images.Heal("locate")
			e.store.Heal("save")
			require.NoError(t, e.svc.Create(context.Background(), "m", "r2", baseSpec()))
		})
	}

	t.Run("undoing a failed create reports what it could not undo", func(t *testing.T) {
		e := newEnv()
		e.store.FailAt("save", errBoom)
		e.disks.FailAt("remove", errBoom)
		err := e.svc.Create(context.Background(), "m", "r", baseSpec())
		requireKind(t, err, machine.ErrFailed)
		assert.ErrorContains(t, err, "root disk could not be removed")
	})
}
