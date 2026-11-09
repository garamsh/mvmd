package statedir_test

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

func TestPaths(t *testing.T) {
	const state = "/var/lib/mvmd/machines/m"
	assert.Equal(t, state+"/supervisor.sock", statedir.SupervisorSocket(state))
	assert.Equal(t, state+"/supervisor.log", statedir.SupervisorLog(state))
	assert.Equal(t, state+"/console.sock", statedir.ConsoleSocket(state))
	assert.Equal(t, state+"/run", statedir.Run(state))
	assert.Equal(t, state+"/run/vmm.log", statedir.VMMLog(statedir.Run(state)))
}

func TestOf_IsTheStateDirectoryOfARunDirectory(t *testing.T) {
	const state = "/var/lib/mvmd/machines/m"
	assert.Equal(t, state, statedir.Of(statedir.Run(state)))
}

func TestConsoleSocket_IsBesideTheRunDirectoryAndNotInIt(t *testing.T) {
	const state = "/s"
	run := statedir.Run(state)
	assert.Equal(t, statedir.ConsoleSocket(state), statedir.ConsoleSocket(statedir.Of(run)))
	assert.NotEqual(t, run, filepath.Dir(statedir.ConsoleSocket(state)))
}
