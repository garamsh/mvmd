package supervision_test

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func linkCount(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	return uint64(st.Nlink)
}

// RemoveState removes the state directory of a Machine whose supervisor has
// ended, and with the run directory's link the disk file is back to one name;
// the control is the second Machine's directory, which stays.
func TestRemoveState_RemovesTheDirectoryAndGivesTheDiskLinkBack(t *testing.T) {
	n := newNode(t)
	src := hostFile(t, "disk")
	n.started(t, "m", vmmUser, nil, func(p *machine.ProcessDescription) {
		p.RunFiles = []machine.RunFile{{Name: "linked", Path: src}}
	})
	n.run(t, "other", time.Hour)
	require.Equal(t, uint64(2), linkCount(t, src), "setup: the run directory holds a link")
	require.NoError(t, n.a.Terminate(ctx, "m"))
	require.DirExists(t, filepath.Join(n.dir, "m"))

	require.NoError(t, n.a.RemoveState(ctx, "m"))
	assert.NoDirExists(t, filepath.Join(n.dir, "m"))
	assert.Equal(t, uint64(1), linkCount(t, src), "the link is gone")
	assert.DirExists(t, filepath.Join(n.dir, "other"), "control: another Machine's directory stays")

	require.NoError(t, n.a.RemoveState(ctx, "m"), "a Machine with no directory succeeds")
}

// A Machine whose supervisor lives keeps its directory, and the failure names
// it; the control is the removal that succeeds once the supervisor has ended.
func TestRemoveState_RefusesWhileTheSupervisorLives(t *testing.T) {
	n := newNode(t)
	n.run(t, "m", time.Hour)
	dir := filepath.Join(n.dir, "m")

	err := n.a.RemoveState(ctx, "m")
	require.Error(t, err)
	assert.Contains(t, err.Error(), dir)
	assert.DirExists(t, dir)

	require.NoError(t, n.a.Terminate(ctx, "m"))
	require.NoError(t, n.a.RemoveState(ctx, "m"))
	assert.NoDirExists(t, dir)
}

// ListState lists the Machines with a state directory, running or not.
func TestListState_ListsEveryStateDirectory(t *testing.T) {
	n := newNode(t)
	got, err := n.a.ListState(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	n.run(t, "running", time.Hour)
	n.run(t, "ended", time.Hour)
	require.NoError(t, n.a.Terminate(ctx, "ended"))
	got, err = n.a.ListState(ctx)
	require.NoError(t, err)
	assert.ElementsMatch(t, []machine.MachineID{"running", "ended"}, got)
}

// A path that is not one directory name is refused.
func TestRemoveState_RefusesAnIdentifierThatIsNotADirectoryName(t *testing.T) {
	n := newNode(t)
	require.Error(t, n.a.RemoveState(ctx, "../x"))
	require.Error(t, n.a.RemoveState(ctx, ""))
}
