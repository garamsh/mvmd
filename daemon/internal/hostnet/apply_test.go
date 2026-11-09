package hostnet_test

import (
	"context"
	"testing"

	"github.com/google/nftables"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// chainsOf lists the names of the chains the adapter's tables hold.
func chainsOf(t *testing.T) (netdev, inet []string) {
	t.Helper()
	c, err := nftables.New()
	require.NoError(t, err)
	chains, err := c.ListChains()
	require.NoError(t, err)
	for _, ch := range chains {
		if ch.Table.Name != "mvmd" {
			continue
		}
		if ch.Table.Family == nftables.TableFamilyNetdev {
			netdev = append(netdev, ch.Name)
		} else {
			inet = append(inet, ch.Name)
		}
	}
	return netdev, inet
}

func linkNamed(t *testing.T, name string) netlink.Link {
	t.Helper()
	l, err := netlink.LinkByName(name)
	if _, missing := err.(netlink.LinkNotFoundError); missing {
		return nil
	}
	require.NoError(t, err)
	return l
}

func TestApply_MakesATapOwnedByTheMachinesUserWithRulesAndTheLinkUp(t *testing.T) {
	inNamespace(t, func(t *testing.T) {
		l := newLab(t, dualStack())
		g := l.start("m", machine.NetworkPolicy{})

		link := linkNamed(t, g.tap)
		require.NotNil(t, link)
		tap, ok := link.(*netlink.Tuntap)
		require.True(t, ok, "the device is a tap, not %s", link.Type())
		assert.Equal(t, netlink.TUNTAP_MODE_TAP, tap.Mode)
		assert.Equal(t, vmmUser.UID, tap.Owner)
		assert.Equal(t, vmmUser.GID, tap.Group)
		assert.NotZero(t, link.Attrs().RawFlags&unix.IFF_UP, "the link is up once the rules are committed")

		netdev, inet := chainsOf(t)
		assert.Contains(t, netdev, g.tap)
		assert.Contains(t, inet, "eg_"+g.tap)
		assert.Contains(t, inet, "in_"+g.tap)

		again, err := l.net.Apply(context.Background(), g.id, vmmUser, machine.NetworkPolicy{})
		require.NoError(t, err)
		assert.Equal(t, g.tap, again, "repeating Apply returns the same tap")
		netdev2, inet2 := chainsOf(t)
		assert.ElementsMatch(t, netdev, netdev2, "and changes nothing")
		assert.ElementsMatch(t, inet, inet2)
	})
}
