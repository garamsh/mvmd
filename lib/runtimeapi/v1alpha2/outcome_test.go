package v1alpha2_test

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A read of exec output before the held window is answered with the earliest
// offset held, as data a client resumes from; any other failure carries none.
func TestEarliestOffset(t *testing.T) {
	gone := v1alpha2.NewOutputGone("the output before offset 4 is no longer held", 4)

	offset, ok := v1alpha2.EarliestOffset(gone)
	assert.True(t, ok)
	assert.Equal(t, uint64(4), offset)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(gone), "it is still a failed precondition")

	offset, ok = v1alpha2.EarliestOffset(v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, "other"))
	assert.False(t, ok, "control: a failed precondition without the detail has no offset")
	assert.Zero(t, offset)
	_, ok = v1alpha2.EarliestOffset(errors.New("plain"))
	assert.False(t, ok)
	_, ok = v1alpha2.EarliestOffset(nil)
	assert.False(t, ok)

	zero := v1alpha2.NewOutputGone("empty window", 0)
	offset, ok = v1alpha2.EarliestOffset(zero)
	assert.True(t, ok, "an earliest offset of zero is still an answer")
	assert.Zero(t, offset)
}
