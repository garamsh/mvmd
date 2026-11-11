package diskfile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// TestLocateAndRemove_RejectInvalidID verifies that Locate and Remove
// refuse an empty DiskID, ".", "..", and names containing "/" — they
// never touch the filesystem for these inputs. The control is an
// absent-valid ID that also succeeds.
func TestLocateAndRemove_RejectInvalidID(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	// Create a sibling file to verify the directory is untouched.
	sibling := filepath.Join(dir, "sibling.txt")
	require.NoError(t, os.WriteFile(sibling, []byte("keep"), 0o644))

	// Verify directory and sibling exist before the tests.
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "directory has exactly one sibling before tests")

	tests := []struct {
		name string
		id   machine.DiskID
	}{
		{"empty", ""},
		{"dot", "."},
		{"dotdot", ".."},
		{"slash", "a/b"},
	}

	for _, tt := range tests {
		t.Run("Locate_"+tt.name, func(t *testing.T) {
			_, err := d.Locate(context.Background(), tt.id)
			assert.Error(t, err, "Locate(%q) must return an error for %q", tt.name, tt.id)
		})

		t.Run("Remove_"+tt.name, func(t *testing.T) {
			err := d.Remove(context.Background(), tt.id)
			assert.Error(t, err, "Remove(%q) must return an error for %q", tt.name, tt.id)
		})
	}

	// Verify the directory and sibling are untouched.
	entries, err = os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "directory and sibling are untouched after invalid IDs")

	// Control: absent-valid ID succeeds (absent is success for Remove).
	t.Run("Control_absentValid", func(t *testing.T) {
		_, err := d.Locate(context.Background(), "absent-root")
		assert.ErrorIs(t, err, machine.ErrDiskAbsent)
		require.NoError(t, d.Remove(context.Background(), "absent-root"))
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		assert.Len(t, entries, 1, "control: absent-valid ID is harmless")
	})
}
