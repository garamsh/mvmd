package app_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/user"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/lib/runtimeapi"
)

// otherGroup returns a group of the current user that is not its primary one,
// by name and ID, or the primary one when there is no other.
func otherGroup(t *testing.T) (string, int) {
	t.Helper()
	gids, err := os.Getgroups()
	require.NoError(t, err)
	for _, gid := range gids {
		if gid == os.Getgid() {
			continue
		}
		if g, err := user.LookupGroupId(strconv.Itoa(gid)); err == nil {
			return g.Name, gid
		}
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	require.NoError(t, err)
	return g.Name, os.Getgid()
}

func TestSocket_CreatedWithTheConfiguredModeAndGroup(t *testing.T) {
	cfg := testConfig(t)
	cfg.Socket.Mode = "0640"
	cfg.Socket.Group, _ = otherGroup(t)
	_, wantGID := otherGroup(t)
	start(t, cfg)

	info, err := os.Lstat(cfg.Socket.Path)
	require.NoError(t, err)
	assert.Equal(t, fs.ModeSocket, info.Mode().Type())
	assert.Equal(t, fs.FileMode(0o640), info.Mode().Perm())
	assert.Equal(t, uint32(wantGID), info.Sys().(*syscall.Stat_t).Gid)
}

// The server side of lib/runtimeapi's client test: a client the socket's
// permissions do not allow is refused, and the same client is let in when the
// mode allows it.
func TestSocket_RefusesAClientWithoutPermission(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses socket permissions")
	}
	cfg := testConfig(t)
	cfg.Socket.Mode = "0000"
	n := serve(t, cfg)
	info, err := os.Lstat(cfg.Socket.Path)
	require.NoError(t, err)
	require.Equal(t, fs.FileMode(0), info.Mode().Perm(), "the socket has the mode that was configured")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = runtimeapi.Dial(ctx, cfg.Socket.Path)
	require.Error(t, err)
	assert.True(t, errors.Is(err, fs.ErrPermission), "got %v", err)

	// Control: opened up, the same dial succeeds, so the mode was the cause.
	require.NoError(t, os.Chmod(cfg.Socket.Path, 0o600))
	conn, err := runtimeapi.Dial(ctx, cfg.Socket.Path)
	require.NoError(t, err)
	require.NoError(t, conn.Close())
	n.shutdown(t)
}

func TestSocket_ReplacesAStaleSocketAndRefusesOtherFiles(t *testing.T) {
	cfg := testConfig(t)
	first := start(t, cfg)
	first.shutdown(t)
	// A socket file a crashed run left behind.
	require.NoError(t, os.WriteFile(cfg.Socket.Path, nil, 0o600))

	a, err := app.New(cfg)
	require.NoError(t, err)
	err = a.Serve(context.Background())
	require.ErrorContains(t, err, "is not a socket")
}

func TestSocket_UnknownGroupIsRefused(t *testing.T) {
	cfg := testConfig(t)
	cfg.Socket.Group = "no-such-group-mvmd"
	a, err := app.New(cfg)
	require.NoError(t, err)
	require.ErrorContains(t, a.Serve(context.Background()), "socket.group")
}
