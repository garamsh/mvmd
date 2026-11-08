package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// TestUIDAllocation_MaxRangeAllocatesOne verifies that a range of
// {First: 1, Count: MaxUint32} — which Validate accepts — still
// allocates a UID. Without the offset fix the loop never runs and
// the first Create fails with exhaustion.
func TestUIDAllocation_MaxRangeAllocatesOne(t *testing.T) {
	ctx := context.Background()
	e := newEnvWithRange(machine.UIDRange{First: 1, Count: ^uint32(0)})

	// The range is valid but wraps with the old loop. It should still
	// allocate one Machine.
	err := e.svc.Create(ctx, "x", request("x"), baseSpec())
	require.NoError(t, err,
		"the highest range Validate accepts must still allocate a Machine")
}
