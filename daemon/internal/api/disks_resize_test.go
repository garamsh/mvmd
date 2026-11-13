package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// resizer is a machine.Service whose ResizeDisk answers a fixed error and
// records what it was asked.
type resizer struct {
	machine.Service
	err   error
	asked []machine.DiskEntry
}

func (r *resizer) ResizeDisk(_ context.Context, disk machine.DiskID, size uint64) error {
	r.asked = append(r.asked, machine.DiskEntry{ID: disk, SizeBytes: size})
	return r.err
}

func TestResizeDisk_PassesTheDiskAndTheSize(t *testing.T) {
	m := &resizer{}

	resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ResizeDisk(context.Background(), &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: 4 << 20})

	require.NoError(t, err)
	assert.NotNil(t, resp)
	assert.Equal(t, []machine.DiskEntry{{ID: "data", SizeBytes: 4 << 20}}, m.asked)
}

func TestResizeDisk_RejectsAnEmptyDiskAndAZeroSizeBeforeTheUseCase(t *testing.T) {
	tests := []struct {
		name string
		req  *v1alpha2.ResizeDiskRequest
	}{
		{"no disk", &v1alpha2.ResizeDiskRequest{SizeBytes: 1}},
		{"a zero size", &v1alpha2.ResizeDiskRequest{DiskId: "data"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &resizer{}

			_, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ResizeDisk(context.Background(), tt.req)

			assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
			assert.Empty(t, m.asked, "the use case was not asked")
		})
	}
}

func TestResizeDisk_MapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"success", nil, v1alpha2.OutcomeKind_OUTCOME_KIND_SUCCESS},
		{"smaller", machine.NewError(machine.ErrInvalid, "smaller"), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
		{"unknown", machine.NewError(machine.ErrNotFound, "no disk"), v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND},
		{"the holder's state", machine.NewError(machine.ErrPreconditionFailed, "paused"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		{"failure", machine.NewError(machine.ErrFailed, "boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &resizer{err: tt.err}

			_, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ResizeDisk(context.Background(), &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: 1})

			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
		})
	}
}

func TestGetRuntimeInfo_ReportsDiskResizeAsItsOwnWireValue(t *testing.T) {
	assert.Equal(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_DISK_RESIZE},
		runtimeInfo(t, machine.CapabilityDiskResize))
	assert.NotContains(t, runtimeInfo(t, machine.CapabilityDiskHotplug), v1alpha2.Capability_CAPABILITY_DISK_RESIZE)
}
