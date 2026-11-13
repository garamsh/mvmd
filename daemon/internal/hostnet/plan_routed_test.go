package hostnet

import (
	"bytes"
	"net/netip"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlan_NamesTheMachinesRoutedPrefixesAtItsTapAndItsPostroutingRule(t *testing.T) {
	r4, r6 := netip.MustParsePrefix("203.0.113.0/28"), netip.MustParsePrefix("2001:db8:100::/56")
	m := testMachine(true, true)
	m.node4, m.node6 = netip.MustParsePrefix("10.77.0.0/24"), netip.MustParsePrefix("fd77::/64")
	m.routed = []netip.Prefix{r4, r6}

	msgs := planOf(t, m)
	var tap []byte
	for _, r := range rulesIn(msgs, netdev, "mvmdtest") {
		tap = append(tap, r.raw...)
	}
	for _, p := range m.routed {
		assert.True(t, bytes.Contains(tap, p.Addr().AsSlice()), "the tap admits a source in %s", p)
	}
	post := rulesIn(msgs, inet, "postrouting")
	require.Len(t, post, 2, "a masquerade rule for each routed prefix")
	for i, p := range m.routed {
		assert.True(t, bytes.Contains(post[i].raw, p.Addr().AsSlice()), "rule %d names %s", i, p)
		assert.Equal(t, m.tap, post[i].userdata, "tagged with the tap, so removal finds it")
	}

	bare := planOf(t, testMachine(true, true))
	var bareTap []byte
	for _, r := range rulesIn(bare, netdev, "mvmdtest") {
		bareTap = append(bareTap, r.raw...)
	}
	assert.False(t, bytes.Contains(bareTap, r4.Addr().AsSlice()), "control: a Machine with no routed prefix admits none")
	assert.Empty(t, rulesIn(bare, inet, "postrouting"), "control: and adds no postrouting rule")
}
