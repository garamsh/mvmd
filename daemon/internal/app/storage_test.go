package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

func TestStorage_TheFirecrackerNodeKeepsDisksAndSnapshotsInItsStateDirectory(t *testing.T) {
	memory := testConfig(t)
	_, err := app.New(memory)
	require.NoError(t, err)
	for _, name := range []string{"disks", "snapshots"} {
		assert.NoDirExists(t, filepath.Join(memory.StateDir, name), "control: the memory config opens no "+name)
	}

	cfg := firecrackerConfig(t)
	_, err = app.New(cfg)
	require.NoError(t, err)
	info, err := os.Stat(filepath.Join(cfg.StateDir, "disks"))
	require.NoError(t, err, "the Disk keeps its files in disks")
	assert.Equal(t, os.FileMode(0o700), info.Mode().Perm(), "only the daemon lists the root disks")
	assert.DirExists(t, filepath.Join(cfg.StateDir, "snapshots"), "the Snapshot Store keeps its files in snapshots")
	assert.DirExists(t, filepath.Join(cfg.StateDir, "snapshots", "staging"))
}

func TestStorage_ANodeWhoseStorageCannotBeOpenedDoesNotStart(t *testing.T) {
	tests := []struct {
		name   string
		damage func(t *testing.T, cfg *app.Config) (repair func())
		reason string
	}{
		{"a disk directory that is a file", occupy("disks"), "the disk directory"},
		{"a snapshot directory that is a file", occupy("snapshots"), "the Snapshot Store"},
		{"a mkfs.ext4 that fails its check", func(t *testing.T, cfg *app.Config) func() {
			good, err := os.ReadFile(cfg.Disk.MKFS)
			require.NoError(t, err)
			require.NoError(t, os.WriteFile(cfg.Disk.MKFS, []byte("#!/bin/sh\nexit 1\n"), 0o700))
			return func() { require.NoError(t, os.WriteFile(cfg.Disk.MKFS, good, 0o700)) }
		}, "mkfs.ext4 not available"},
		{"a mkfs.ext4 that is not there", func(t *testing.T, cfg *app.Config) func() {
			good := cfg.Disk.MKFS
			cfg.Disk = &app.DiskConfig{MKFS: filepath.Join(filepath.Dir(good), "absent")}
			return func() { cfg.Disk = &app.DiskConfig{MKFS: good} }
		}, "mkfs.ext4 not available"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := firecrackerConfig(t)
			require.NoError(t, os.MkdirAll(cfg.StateDir, 0o700))
			repair := tt.damage(t, &cfg)

			_, err := app.New(cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)

			// The control: with only that input repaired the same node starts,
			// and the refusal above closed the database it had opened.
			repair()
			_, err = app.New(cfg)
			require.NoError(t, err)
		})
	}
}

// occupy puts a regular file where the node would make the directory name.
func occupy(name string) func(t *testing.T, cfg *app.Config) func() {
	return func(t *testing.T, cfg *app.Config) func() {
		path := filepath.Join(cfg.StateDir, name)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		return func() { require.NoError(t, os.Remove(path)) }
	}
}
