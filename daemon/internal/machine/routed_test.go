package machine_test

import (
	"context"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func prefixes(ss ...string) []netip.Prefix {
	var out []netip.Prefix
	for _, s := range ss {
		out = append(out, netip.MustParsePrefix(s))
	}
	return out
}

// routedSpec is the base spec with routed prefixes.
func routedSpec(ss ...string) machine.Spec {
	spec := baseSpec()
	spec.RoutedPrefixes = prefixes(ss...)
	return spec
}

func TestCreate_ARoutedPrefixOutsideTheRulesIsRejectedNamingItAndHoldsNothing(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		prefixes []string
		names    string
	}{
		{"five prefixes", []string{"198.51.100.0/24", "198.51.101.0/24", "198.51.102.0/24", "198.51.103.0/24", "198.51.104.0/24"}, "at most 4"},
		{"IPv4 shorter than /16", []string{"198.0.0.0/15"}, "198.0.0.0/15"},
		{"IPv6 shorter than /48", []string{"2001:db8::/47"}, "2001:db8::/47"},
		{"host bits set", []string{"198.51.100.5/24"}, "198.51.100.5/24"},
		{"loopback", []string{"127.0.0.0/16"}, "127.0.0.0/16"},
		{"link-local", []string{"169.254.0.0/16"}, "169.254.0.0/16"},
		{"IPv6 link-local", []string{"fe80::/64"}, "fe80::/64"},
		{"multicast", []string{"224.0.0.0/16"}, "224.0.0.0/16"},
		{"unspecified", []string{"0.0.0.0/16"}, "0.0.0.0/16"},
		{"IPv4-mapped IPv6", []string{"::ffff:198.51.100.0/120"}, "::ffff:198.51.100.0/120"},
		{"the same prefix twice", []string{"198.51.100.0/24", "198.51.100.0/24"}, "198.51.100.0/24"},
		{"one inside another", []string{"198.51.0.0/16", "198.51.100.0/24"}, "198.51.100.0/24"},
		{"an invalid prefix", nil, "not a prefix"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			spec := routedSpec(tc.prefixes...)
			if tc.prefixes == nil {
				spec.RoutedPrefixes = []netip.Prefix{{}}
			}
			err := e.svc.Create(ctx, "m", request("m"), spec)
			requireKind(t, err, machine.ErrInvalid)
			assert.Contains(t, err.Error(), tc.names)
			e.requireNoMachine(t, "m")
			assert.False(t, e.net.Allocated("m"), "nothing was assigned")
			_, held := e.store.Record("m")
			assert.False(t, held, "a request outside the rules is not recorded")

			// The control: the same Machine with prefixes inside the rules.
			require.NoError(t, e.svc.Create(ctx, "m", request("m"), routedSpec("198.51.100.0/24", "2001:db8:100::/48", "203.0.113.7/32")))
		})
	}
}

func TestCreate_APrefixAnotherMachineHoldsIsAConflictNamingItAndHoldsNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	require.NoError(t, e.svc.Create(ctx, "first", request("first"), routedSpec("198.51.100.0/24")))

	err := e.svc.Create(ctx, "second", request("second"), routedSpec("198.51.100.128/25"))
	requireKind(t, err, machine.ErrConflict)
	assert.Contains(t, err.Error(), "198.51.100.128/25")
	e.requireNoMachine(t, "second")
	assert.False(t, e.net.Allocated("second"), "nothing was assigned")
	_, held := e.store.Record("second")
	assert.False(t, held, "a request the node refuses is not recorded")

	// The control: a prefix that overlaps none is accepted, and the refused
	// Machine may be created with it under the same identifier.
	require.NoError(t, e.svc.Create(ctx, "second", request("second"), routedSpec("203.0.113.0/24")))
}

func TestCreate_TheRoutedPrefixesAreFixedAndReleasedAtDelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	spec := routedSpec("198.51.100.0/24", "2001:db8:100::/48")
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))

	m, err := e.svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, prefixes("198.51.100.0/24", "2001:db8:100::/48"), m.Network.RoutedPrefixes, "Inspect reports what the node holds")
	rec, ok := e.store.Record("m")
	require.True(t, ok)
	assert.Equal(t, m.Network.RoutedPrefixes, rec.Machine.Network.RoutedPrefixes, "the record holds them")

	require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec), "repeating the call is no change")
	other := routedSpec("198.51.100.0/24")
	requireKind(t, e.svc.Create(ctx, "m", request("m"), other), machine.ErrConflict)
	m, err = e.svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Len(t, m.Network.RoutedPrefixes, 2, "a repeat with other prefixes changes nothing")

	requireKind(t, e.svc.Create(ctx, "n", request("n"), routedSpec("198.51.100.0/24")), machine.ErrConflict)
	require.NoError(t, e.svc.Delete(ctx, "m"))
	require.NoError(t, e.svc.Create(ctx, "n", request("n"), routedSpec("198.51.100.0/24")), "Delete released the prefix")
}

func TestRestoreSnapshot_RoutedPrefixesAreTheRequestsAndHeldLikeCreates(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.Create(ctx, "holder", request("holder"), routedSpec("198.51.100.0/24")))

	t.Run("a prefix outside the rules", func(t *testing.T) {
		err := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy, netip.MustParsePrefix("198.51.100.5/24"))
		requireKind(t, err, machine.ErrInvalid)
		assert.Contains(t, err.Error(), "198.51.100.5/24")
		e.requireNoMachine(t, "new")
	})
	t.Run("a prefix another Machine holds", func(t *testing.T) {
		err := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy, netip.MustParsePrefix("198.51.100.0/25"))
		requireKind(t, err, machine.ErrConflict)
		assert.Contains(t, err.Error(), "198.51.100.0/25")
		e.requireNoMachine(t, "new")
		assert.False(t, e.net.Allocated("new"), "nothing was assigned")
	})
	t.Run("control: a prefix that is free", func(t *testing.T) {
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy, netip.MustParsePrefix("203.0.113.0/24")))
		m, err := e.svc.Inspect(ctx, "new")
		require.NoError(t, err)
		assert.Equal(t, prefixes("203.0.113.0/24"), m.Network.RoutedPrefixes)
		assert.Equal(t, prefixes("203.0.113.0/24"), m.RoutedPrefixes)

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy, netip.MustParsePrefix("203.0.113.0/24")), "repeating it is no change")
		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), machine.ErrConflict) // a repeat with no prefixes is another request
	})
}
