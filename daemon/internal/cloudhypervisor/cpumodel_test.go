package cloudhypervisor_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
)

func TestCPUModel_NamesTheFirstProcessorByItsIdentity(t *testing.T) {
	x86 := `processor	: 0
vendor_id	: GenuineIntel
cpu family	: 6
model		: 183
model name	: Intel(R) Core(TM) i7
stepping	: 1
MHz		: 3892.684

processor	: 1
vendor_id	: GenuineIntel
cpu family	: 6
model		: 999
stepping	: 7
`
	got, err := cloudhypervisor.CPUModel(strings.NewReader(x86))
	require.NoError(t, err)
	assert.Equal(t, "vendor_id=GenuineIntel cpu family=6 model=183 stepping=1", got, "the first processor, and not the model name or MHz")

	arm := `processor	: 0
CPU implementer	: 0x41
CPU architecture: 8
CPU variant	: 0x3
CPU part	: 0xd0c
CPU revision	: 1
`
	got, err = cloudhypervisor.CPUModel(strings.NewReader(arm))
	require.NoError(t, err)
	assert.Equal(t, "CPU implementer=0x41 CPU architecture=8 CPU variant=0x3 CPU part=0xd0c CPU revision=1", got)
}

func TestCPUModel_RefusesADescriptionThatNamesNoModel(t *testing.T) {
	_, err := cloudhypervisor.CPUModel(strings.NewReader("processor : 0\nbogomips : 1\n"))
	require.Error(t, err)
}
