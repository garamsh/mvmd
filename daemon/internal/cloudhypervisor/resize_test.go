package cloudhypervisor_test

import (
	"context"
	"encoding/json"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// vmmSizing models what the VMM holds of a Machine's size: the vCPUs and the
// memory it is configured with, which a resize changes at once, and the memory
// the guest has plugged, which follows only as the guest takes part.
type vmmSizing struct {
	mu                sync.Mutex
	bootVCPUs         uint32
	size, hotplugged  uint64
	actual            uint64
	ceilingVCPUs      uint32
	ceilingMemory     uint64
	pendingVCPURemove bool
	// plug, when set, is asked at each GET /vm.info with how many have been
	// answered since the resize, and returns the memory actually plugged.
	plug  func(polls int) uint64
	polls int
	sized bool
}

func vmmSized(f *fakeAPI, vcpus uint32, size, ceilingMemory uint64) *vmmSizing {
	v := &vmmSizing{bootVCPUs: vcpus, size: size, actual: size, ceilingVCPUs: 8, ceilingMemory: ceilingMemory}
	v.plug = func(int) uint64 { return v.size + v.hotplugged }
	f.answer = v.answer
	return v
}

func (v *vmmSizing) answer(r request) (int, string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	switch {
	case r.Method == "PUT" && r.Path == "/vm.resize":
		var body struct {
			VCPUs *uint32 `json:"desired_vcpus"`
			RAM   *uint64 `json:"desired_ram"`
		}
		_ = json.Unmarshal([]byte(r.Body), &body)
		if body.VCPUs != nil {
			if v.pendingVCPURemove || *body.VCPUs > v.ceilingVCPUs || *body.VCPUs < 1 {
				return http.StatusInternalServerError, "the vCPU count is refused"
			}
			v.bootVCPUs = *body.VCPUs
		}
		if body.RAM != nil {
			if *body.RAM > v.ceilingMemory || *body.RAM < v.size || *body.RAM%(2<<20) != 0 {
				return http.StatusInternalServerError, "the memory is refused"
			}
			v.hotplugged = *body.RAM - v.size
		}
		v.sized = true
	case r.Method == "GET" && r.Path == "/vm.info":
		if v.sized {
			v.polls++
			v.actual = v.plug(v.polls)
		}
		b, _ := json.Marshal(map[string]any{
			"state": "Running",
			"config": map[string]any{
				"cpus":   map[string]any{"boot_vcpus": v.bootVCPUs},
				"memory": map[string]any{"size": v.size, "hotplugged_size": v.hotplugged},
			},
			"memory_actual_size": v.actual,
		})
		return http.StatusOK, string(b)
	}
	return http.StatusNoContent, ""
}

func (v *vmmSizing) set(f func(*vmmSizing)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f(v)
}

func requireResizes(t *testing.T) {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skip("resize is x86_64's")
	}
}

func resizeBodies(reqs []request) []string {
	var out []string
	for _, r := range reqs {
		if r.Method == "PUT" && r.Path == "/vm.resize" {
			out = append(out, r.Body)
		}
	}
	return out
}

func TestResize_SendsTheDesiredFiguresAndReturnsOnceTheVMMReportsThem(t *testing.T) {
	requireResizes(t)
	tests := []struct {
		name   string
		target machine.Resources
		body   string
	}{
		{"both figures", machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}, `{"desired_ram":536870912,"desired_vcpus":4}`},
		{"the vCPUs only", machine.Resources{VCPUs: 4, MemoryBytes: 256 << 20}, `{"desired_vcpus":4}`},
		{"the memory only", machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}, `{"desired_ram":536870912}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n := newNode(t)
			f := &fakeAPI{}
			n.booted(f)
			vmmSized(f, 2, 256<<20, 1<<30)
			before := len(f.seen())

			require.NoError(t, n.driver.Resize(context.Background(), "m1", tt.target))

			assert.Equal(t, []string{tt.body}, resizeBodies(since(f, before)), "only the figures that change are sent")
			assert.Greater(t, countOf(since(f, before), "GET", "/vm.info"), 1, "and the VMM was asked what the guest reached")
		})
	}
}

// The VMM answers a resize before the guest has plugged the memory, and
// memory_actual_size is what the guest has reached, so a resize returns after
// the figure has caught up and not before.
func TestResize_WaitsForTheGuestToPlugTheMemory(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	v := vmmSized(f, 2, 256<<20, 1<<30)
	v.set(func(v *vmmSizing) {
		v.plug = func(polls int) uint64 {
			if polls >= 4 {
				return 512 << 20
			}
			return 256 << 20
		}
	})

	require.NoError(t, n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}))

	assert.GreaterOrEqual(t, v.polls, 4, "asked again until the guest had plugged it")
}

func TestResize_ReportsAGuestThatDoesNotReachTheTargetAsAnError(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	v := vmmSized(f, 2, 256<<20, 1<<30)
	v.set(func(v *vmmSizing) { v.plug = func(int) uint64 { return 256 << 20 } })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := n.driver.Resize(ctx, "m1", machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "asked for")
}

// A resize that timed out has already changed the VMM's configuration, and the
// VMM refuses a vCPU resize while a removal is pending. The retry reads the
// configuration, sends nothing for what is already configured, and waits.
func TestResize_ARetryAfterATimedOutWaitSendsNothingAndConverges(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	v := vmmSized(f, 4, 256<<20, 1<<30)
	v.set(func(v *vmmSizing) { v.plug = func(int) uint64 { return 256 << 20 } })
	target := machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	before := len(f.seen())

	require.Error(t, n.driver.Resize(ctx, "m1", target))
	assert.Len(t, resizeBodies(since(f, before)), 1, "control: the first call sent the resize")

	v.set(func(v *vmmSizing) {
		v.pendingVCPURemove = true // the VMM would refuse the same vCPU request again
		v.plug = func(int) uint64 { return v.size + v.hotplugged }
	})
	mid := len(f.seen())
	require.NoError(t, n.driver.Resize(context.Background(), "m1", target))

	assert.Empty(t, resizeBodies(since(f, mid)), "the VMM is already configured for the target, so nothing is sent")
}

func TestResize_AMachineAlreadyAtTheTargetSendsNothing(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	vmmSized(f, 2, 256<<20, 1<<30)
	before := len(f.seen())

	require.NoError(t, n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}))

	assert.Empty(t, resizeBodies(since(f, before)))
}

// A shrink is the same call with smaller figures; the VMM's memory goes back
// down to the boot size at the least.
func TestResize_ShrinksByTheSameCall(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	v := vmmSized(f, 4, 256<<20, 1<<30)
	v.set(func(v *vmmSizing) { v.hotplugged = 256 << 20; v.actual = 512 << 20 })
	before := len(f.seen())

	require.NoError(t, n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}))

	assert.Equal(t, []string{`{"desired_ram":268435456,"desired_vcpus":2}`}, resizeBodies(since(f, before)))
}

func TestResize_RefusesMemoryThatIsNotAMultipleOf2MiB(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	vmmSized(f, 2, 256<<20, 1<<30)
	before := len(f.seen())

	err := n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 2, MemoryBytes: 257 << 20})
	require.ErrorIs(t, err, machine.ErrInvalid)
	assert.Len(t, f.seen(), before, "nothing reached the VMM")

	require.NoError(t, n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 2, MemoryBytes: 258 << 20}),
		"control: a multiple of 2 MiB is accepted")
	assert.Len(t, resizeBodies(since(f, before)), 1)
}

func TestResize_ReturnsTheVMMsRefusal(t *testing.T) {
	requireResizes(t)
	n := newNode(t)
	f := &fakeAPI{}
	n.booted(f)
	vmmSized(f, 2, 256<<20, 1<<30)

	err := n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 9, MemoryBytes: 256 << 20})
	require.Error(t, err, "the VMM's maximum is 8 vCPUs")
	assert.Contains(t, err.Error(), "refused")

	require.NoError(t, n.driver.Resize(context.Background(), "m1", machine.Resources{VCPUs: 8, MemoryBytes: 256 << 20}),
		"control: the maximum itself is accepted")
}
