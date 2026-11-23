package app_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

// mvmd -backup copies the state directory's database to a new file and exits; the copy is
// refused while another mvmd runs on the directory (ADR 0071).
func TestMain_BackupCopiesTheDatabaseOfAStoppedNode(t *testing.T) {
	cfg := testConfig(t)
	raw := []byte(`{"socket":{"path":"` + cfg.Socket.Path + `","mode":"0600"},"state_dir":"` + cfg.StateDir + `","driver":"memory","vmm_uid_range":{"first":1000,"count":65536}}`)
	file := filepath.Join(t.TempDir(), "mvmd.json")
	require.NoError(t, os.WriteFile(file, raw, 0o600))
	dest := filepath.Join(t.TempDir(), "mvmd-backup.db")

	running := start(t, cfg)
	var out bytes.Buffer
	assert.Equal(t, 1, app.Main([]string{"-backup", dest, "-config", file}, &out), "a running mvmd holds the database")
	assert.NoFileExists(t, dest)
	running.shutdown(t)

	out.Reset()
	assert.Zero(t, app.Main([]string{"-backup", dest, "-config", file}, &out), out.String())
	assert.Contains(t, out.String(), "wrote")
	assert.FileExists(t, dest)
}
