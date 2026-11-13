package cloudhypervisor_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Cloud Hypervisor v54.0 resizes a raw disk with PUT /vm.resize-disk and sets
// the length of the file itself (ADR 0055), so one request is all the Driver sends.
func TestResizeDisk_AsksTheVMMToResizeTheDiskAndReportsTheCapability(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	require.NoError(t, n.driver.ResizeDisk(context.Background(), "m1", "data", 8<<20))

	got := since(f, before)
	require.Len(t, got, 1, "one request")
	assert.Equal(t, "PUT", got[0].Method)
	assert.Equal(t, "/vm.resize-disk", got[0].Path)
	assert.JSONEq(t, `{"id":"`+dataDrive+`","desired_size":8388608}`, got[0].Body)
	assert.Contains(t, n.driver.Info().Capabilities, machine.CapabilityDiskResize)
}

// A size the VMM would refuse is refused before it is sent. The control is a
// multiple of 512, which is sent.
func TestResizeDisk_ASizeThatIsNotAMultipleOf512IsRefusedBeforeTheVMM(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	before := len(f.seen())

	for _, size := range []uint64{0, 511, 1<<20 + 1} {
		require.Error(t, n.driver.ResizeDisk(context.Background(), "m1", "data", size), "size %d", size)
	}
	require.Error(t, n.driver.ResizeDisk(context.Background(), "m1", "", 1<<20))
	assert.Empty(t, since(f, before), "the VMM was asked nothing")

	require.NoError(t, n.driver.ResizeDisk(context.Background(), "m1", "data", 1<<20+512))
	assert.Len(t, since(f, before), 1)
}

func TestResizeDisk_ARefusalOfTheVMMIsAnError(t *testing.T) {
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	f.answer = func(r request) (int, string) {
		if r.Path == "/vm.resize-disk" {
			return http.StatusInternalServerError, "the disk is read-only"
		}
		return http.StatusNoContent, ""
	}

	err := n.driver.ResizeDisk(context.Background(), "m1", "data", 8<<20)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "the disk is read-only")
}
