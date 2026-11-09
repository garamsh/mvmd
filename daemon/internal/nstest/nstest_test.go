package nstest

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestInFreshNamespace is the test the others rerun: it holds in a namespace
// made for it and nowhere else.
func TestInFreshNamespace(t *testing.T) {
	Run(t, func(t *testing.T) {
		assert.Equal(t, "1", os.Getenv(inNamespaceEnv))
		assert.Zero(t, os.Geteuid(), "root of the namespace")
	})
}

func TestRerun_ReportsATestThatPassedAndRefusesOneThatRanNothing(t *testing.T) {
	self, err := os.Executable()
	require.NoError(t, err)

	out, err := rerun(self, "TestInFreshNamespace")
	assert.NoError(t, err, "control: a test that exists passes in a namespace\n%s", out)

	// A name that is a pattern for every test must match none, since it is
	// quoted, and a child that ran nothing exits 0 all the same.
	out, err = rerun(self, "TestInFresh.*")
	assert.Error(t, err, "a pattern that matches no test is not a pass\n%s", out)
	_, err = rerun(self, "TestNoSuchTest")
	assert.Error(t, err)
}
