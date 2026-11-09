package app

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	fencingmemory "github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A conversion's supervisor expires at the end of the conversion limit, counted
// from its start, and the conversions are listed as the supervisors hold them.
func TestConversionSupervision_ExpiresAtTheEndOfTheLimit(t *testing.T) {
	ctx := context.Background()
	clock := fencingmemory.NewClock("boot-1")
	clock.Advance(7 * time.Second)
	sups := fencingmemory.NewSupervisors(nil)
	conv := conversionSupervision{sup: sups, clock: clock, limit: 10 * time.Minute}

	require.NoError(t, conv.Start(ctx, "convert-a", machine.ProcessDescription{}, machine.User{UID: 1000, GID: 1000}))
	expiry, ok := sups.Expiry("convert-a")
	require.True(t, ok)
	assert.Equal(t, clock.Now().Add(10*time.Minute), expiry, "the expiry is the limit from the start")

	clock.Advance(time.Minute)
	require.NoError(t, conv.Start(ctx, "convert-b", machine.ProcessDescription{}, machine.User{UID: 1001, GID: 1001}))
	later, _ := sups.Expiry("convert-b")
	assert.Equal(t, expiry.Add(time.Minute), later, "control: the limit is counted from each start, not from a fixed time")

	live, err := conv.Live(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []machine.MachineID{"convert-a", "convert-b"}, live)

	require.NoError(t, conv.Terminate(ctx, "convert-a"))
	live, err = conv.Live(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.MachineID{"convert-b"}, live)
}

func validConversion() ConversionConfig {
	return ConversionConfig{Converter: "converter-1", VCPUs: 2, MemoryBytes: 512 << 20, OutputBytes: 1 << 30, NameSetBytes: 1 << 20, Limit: "10m"}
}

// The section reaches the use case's settings as written.
func TestConversionConfig_SettingsCarryTheSection(t *testing.T) {
	settings, err := validConversion().settings()
	require.NoError(t, err)

	assert.Equal(t, "converter-1", settings.Converter)
	assert.Equal(t, machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}, settings.Resources)
	assert.Equal(t, uint64(1<<30), settings.OutputBytes)
	assert.Equal(t, uint64(1<<20), settings.NameSetBytes)
	assert.Equal(t, 10*time.Minute, settings.Limit)
	assert.Equal(t, "linux", settings.Platform.OS)
	assert.NotEmpty(t, settings.Platform.Architecture)

	bad := validConversion()
	bad.Limit = "soon"
	_, err = bad.settings()
	assert.Error(t, err, "control: a limit that is not a duration is refused, not defaulted")
}
