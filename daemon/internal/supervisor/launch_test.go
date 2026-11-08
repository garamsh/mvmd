package supervisor

import (
	"testing"

	"github.com/stretchr/testify/assert"

	pb "github.com/garamsh/mvmd/daemon/internal/supervisorapi/v1alpha1"
)

// Only a VMM that opens its tap by name shares the host's network namespace:
// one with no network device, as a converter, and one that takes its tap by
// descriptor each get a namespace of their own (ADR 0015).
func TestNewNetworkNamespace_OnlyAVMMThatOpensItsTapByNameSharesTheHosts(t *testing.T) {
	for _, c := range []struct {
		name         string
		tap          string
		byDescriptor bool
		want         bool
	}{
		{"no network device", "", false, true},
		{"a tap by descriptor", "tap0", true, true},
		{"a tap by name", "tap0", false, false},
	} {
		req := &pb.StartRequest{Tap: c.tap, Process: &pb.Process{TapByDescriptor: c.byDescriptor}}
		assert.Equal(t, c.want, newNetworkNamespace(req), c.name)
	}
}
