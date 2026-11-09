package pinnedfile_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
)

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestCheck_AcceptsTheFileOfTheDigestAndRefusesAnyOther(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel")
	require.NoError(t, os.WriteFile(path, []byte("a kernel"), 0o644))

	require.NoError(t, pinnedfile.Check("the kernel", path, sum([]byte("a kernel"))), "control: the pinned file passes")

	err := pinnedfile.Check("the kernel", path, sum([]byte("another kernel")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "the kernel "+path+" has digest "+sum([]byte("a kernel")))
	assert.Contains(t, err.Error(), "not the configured "+sum([]byte("another kernel")))
}

func TestCheck_RefusesAFileThatCannotBeRead(t *testing.T) {
	err := pinnedfile.Check("the binary", filepath.Join(t.TempDir(), "missing"), sum(nil))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.Contains(t, err.Error(), "check the binary")
}
