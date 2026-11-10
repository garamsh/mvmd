package supervision_test

import (
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
	"github.com/garamsh/mvmd/daemon/internal/supervision"
)

// silentSupervisor is a supervisor binary that never binds its socket, so a
// Start with it stays between creating the VMM cgroup and the supervisor
// answering until its context ends.
func silentSupervisor(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(shortDir(t), "silent-supervisor")
	require.NoError(t, os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755))
	return bin
}

// startInProgress starts the Machine with a supervisor that never answers and
// returns once its VMM cgroup exists. The returned function ends the start and
// returns its error.
func startInProgress(t *testing.T, id string) (n *node, end func() error) {
	t.Helper()
	n = newNode(t)
	cfg := adapterConfig(n.dir)
	cfg.Binary = silentSupervisor(t)
	a, err := supervision.New(cfg)
	require.NoError(t, err)
	n.a = a

	startCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() {
		done <- a.Start(startCtx, machine.MachineID(id), vmmProcess(runDir(n.dir, id)), vmmUser, fencing.BootTime(boottime(t)+int64(time.Hour)))
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(vmmCgroup, id))
		return err == nil
	}, 10*time.Second, time.Millisecond, "the start created the VMM cgroup")
	require.NoFileExists(t, statedir.SupervisorSocket(filepath.Join(n.dir, id)), "the supervisor has not bound its socket")
	return n, func() error {
		cancel()
		select {
		case err := <-done:
			return err
		case <-time.After(10 * time.Second):
			t.Fatal("the start did not return after its context ended")
			return nil
		}
	}
}

func TestLive_LeavesTheCgroupOfAMachineWhoseStartIsInProgress(t *testing.T) {
	n, end := startInProgress(t, "m")
	cgroup := filepath.Join(vmmCgroup, "m")

	live, err := n.a.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live, "a supervisor that does not answer yet is not listed")
	assert.DirExists(t, cgroup, "the listing leaves the cgroup the start created")

	// Control: once the start has failed its cgroup is gone, so the cgroup
	// above was kept for the start and not left by accident.
	require.ErrorIs(t, end(), context.Canceled)
	assert.NoDirExists(t, cgroup)
}

// bindWithoutListening binds a stream socket at the path and leaves it
// unlistened, as a supervisor is between its bind and its listen.
func bindWithoutListening(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = syscall.Close(fd) })
	require.NoError(t, syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}))
	require.FileExists(t, path)
}

func TestLive_LeavesTheSocketOfASupervisorThatHasBoundAndNotListened(t *testing.T) {
	n, end := startInProgress(t, "m")
	sock := statedir.SupervisorSocket(filepath.Join(n.dir, "m"))
	bindWithoutListening(t, sock)

	_, err := n.a.Live(ctx)
	require.NoError(t, err)
	assert.FileExists(t, sock, "the listing leaves the socket of a supervisor that is about to listen")
	assert.DirExists(t, filepath.Join(vmmCgroup, "m"))

	require.Error(t, end())
}

func TestLive_RemovesTheSocketAndCgroupOfASupervisorThatEnded(t *testing.T) {
	n := newNode(t)
	_, ns := n.run(t, "m", time.Hour)
	sock := statedir.SupervisorSocket(filepath.Join(n.dir, "m"))
	cgroup := filepath.Join(vmmCgroup, "m")

	// Control: while the supervisor lives it is listed and nothing is removed.
	live, err := n.a.Live(ctx)
	require.NoError(t, err)
	require.Len(t, live, 1)
	require.FileExists(t, sock)
	require.DirExists(t, cgroup)

	require.NoError(t, syscall.Kill(supervisorOf(t, "m"), syscall.SIGKILL))
	require.Eventually(t, func() bool { return len(members(ns)) == 0 }, 10*time.Second, time.Millisecond)

	live, err = n.a.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live)
	assert.NoFileExists(t, sock, "the listing removes the socket of the supervisor that ended")
	assert.NoDirExists(t, cgroup, "and its VMM cgroup")
}
