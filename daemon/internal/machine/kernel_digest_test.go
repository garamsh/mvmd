package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// Inspect reports the kernel the Machine's guest was booted with: a started
// Machine's is the one in its boot description, a restored Machine's is the one
// its snapshot recorded, and a Machine that never booted has none.
func TestInspect_ReportsTheKernelDigestOfTheBoot(t *testing.T) {
	ctx := context.Background()

	t.Run("a Machine that never booted reports none", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateCreated)
		assert.Empty(t, e.requireState(t, "m", machine.StateCreated).KernelDigest)
	})

	t.Run("a started Machine reports the digest of the boot description's kernel", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		assert.Equal(t, memory.KernelDigest, e.requireState(t, "m", machine.StateRunning).KernelDigest)
		e.requireStored(t, "m", machine.StateRunning)
		rec, _ := e.store.Record("m")
		assert.Equal(t, memory.KernelDigest, rec.Machine.KernelDigest, "the record keeps it")
	})

	t.Run("a stopped Machine keeps the digest of the boot it took", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateStopped)
		assert.Equal(t, memory.KernelDigest, e.requireState(t, "m", machine.StateStopped).KernelDigest)
	})

	t.Run("a restored Machine reports the digest its snapshot recorded", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
		assert.Equal(t, memory.KernelDigest, e.requireState(t, "new", machine.StateRunning).KernelDigest)
	})
}
