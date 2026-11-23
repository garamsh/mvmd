package machine_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func requesting(addresses ...string) machine.Spec {
	spec := baseSpec()
	for _, a := range addresses {
		spec.RequestedAddresses = append(spec.RequestedAddresses, netip.MustParseAddr(a))
	}
	return spec
}

// ADR 0069: a Machine created with an address it asked for has that address, an
// address the node cannot give refuses the Create, and with none asked for the
// choice is the node's, as before.
func TestCreate_GivesTheAddressTheClientAsksFor(t *testing.T) {
	ctx := context.Background()
	e := newEnv()

	require.NoError(t, e.svc.Create(ctx, "asked", request("asked"), requesting("10.0.1.50")))
	m := e.requireState(t, "asked", machine.StateCreated)
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.1.50")}, m.Network.Addresses, "the address asked for")
	assert.Equal(t, []netip.Addr{netip.MustParseAddr("10.0.1.50")}, m.RequestedAddresses, "kept with the Machine")

	require.NoError(t, e.svc.Create(ctx, "chosen", request("chosen"), baseSpec()))
	chosen := e.requireState(t, "chosen", machine.StateCreated)
	assert.NotEqual(t, m.Network.Addresses, chosen.Network.Addresses, "control: with nothing asked for the node chooses, and not the one held")
	assert.Empty(t, chosen.RequestedAddresses)

	again := e.requireState(t, "asked", machine.StateCreated)
	assert.Equal(t, m.Network.Addresses, again.Network.Addresses)
}

func TestCreate_RefusesAnAddressItCannotGiveAndMakesNothing(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		asked string
		kind  error
	}{
		{"outside the node's prefix", "192.0.2.9", machine.ErrInvalid},
		{"the network address", "10.0.0.0", machine.ErrInvalid},
		{"the broadcast address", "10.0.255.255", machine.ErrInvalid},
		{"a family the node does not serve", "fd00::5", machine.ErrInvalid},
		{"the gateway", "10.0.255.254", machine.ErrConflict},
		{"an address another Machine holds", "10.0.1.50", machine.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			require.NoError(t, e.svc.Create(ctx, "holder", request("holder"), requesting("10.0.1.50")))

			err := e.svc.Create(ctx, "asker", request("asker"), requesting(tt.asked))

			requireKind(t, err, tt.kind)
			assert.ErrorContains(t, err, tt.asked, "the refusal names the address")
			e.requireNoMachine(t, "asker")
			assert.False(t, e.net.Allocated("asker"), "nothing was taken")
			assert.Equal(t, 1, e.disks.Calls("create-root"), "no root disk was made for it: the holder's is the only one")

			require.NoError(t, e.svc.Create(ctx, "asker", request("asker"), requesting("10.0.1.51")), "control: a free address inside the prefix is given")
		})
	}
}

func TestCreate_AnAddressAskedForIsPartOfTheRequestAndRepeatsAreCompared(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), requesting("10.0.1.50")))

	require.NoError(t, e.svc.Create(ctx, "m", request("m"), requesting("10.0.1.50")), "a repeat with the same address succeeds")
	requireKind(t, e.svc.Create(ctx, "m", request("m"), requesting("10.0.1.51")), machine.ErrConflict)
	requireKind(t, e.svc.Create(ctx, "m", request("m"), baseSpec()), machine.ErrConflict)
	assert.Equal(t, 1, e.net.Calls("allocate"), "nothing was allocated again")
}

func TestCreate_RefusesRequestedAddressesOfABadFormAsInvalid(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name  string
		asked []netip.Addr
	}{
		{"two of one family", []netip.Addr{netip.MustParseAddr("10.0.1.5"), netip.MustParseAddr("10.0.1.6")}},
		{"an IPv4-mapped IPv6 address", []netip.Addr{netip.MustParseAddr("::ffff:10.0.1.5")}},
		{"a zone", []netip.Addr{netip.MustParseAddr("fe80::1%eth0")}},
		{"no address", []netip.Addr{{}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			spec := baseSpec()
			spec.RequestedAddresses = tt.asked

			requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrInvalid)
			e.requireNoMachine(t, "m")
			assert.Zero(t, e.net.Calls("allocate"))
		})
	}
	assert.NoError(t, machine.ValidateRequestedAddresses([]netip.Addr{netip.MustParseAddr("10.0.1.5"), netip.MustParseAddr("fd00::5")}), "control: one of each family is of good form")
}
