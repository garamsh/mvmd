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

type lister struct {
	machine.Service
	all []machine.Machine
}

func (l *lister) List(context.Context) ([]machine.Machine, error) { return l.all, nil }

func TestListMachines_ReportsTheKernelDigestOrNone(t *testing.T) {
	m := &lister{all: []machine.Machine{{ID: "booted", KernelDigest: "abc123"}, {ID: "never"}}}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListMachines(context.Background(), &v1alpha2.ListMachinesRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Machines, 2)
	assert.Equal(t, "abc123", resp.Machines[0].KernelDigest)
	assert.Empty(t, resp.Machines[1].KernelDigest)
}
