package app_test

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// withBootSets is the Firecracker config with the boot_sets section.
func withBootSets(sets string) string {
	raw := rawConfig("firecracker", fcSection+cvSection+dkSection+nwSection, fcSupervise)
	return strings.Replace(raw, `"initrd_sha256":"`+digest64+`"}`+cvSection, `"initrd_sha256":"`+digest64+`","boot_sets":`+sets+`}`+cvSection, 1)
}

func TestBootSets_ConfigParsesAndRefuses(t *testing.T) {
	good := `{"os":{"kernel":{"path":"/opt/os-vmlinux","sha256":"` + digest64 + `"},"initrd":{"path":"/opt/os-initrd","sha256":"` + digest64 + `"}}}`
	require.NotEqual(t, withBootSets(good), rawConfig("firecracker", fcSection+cvSection+dkSection+nwSection, fcSupervise), "the row adds the section")
	cfg, err := app.ParseConfig([]byte(withBootSets(good)))
	require.NoError(t, err, "control: a well-formed boot set")
	assert.Equal(t, map[string]app.BootSetConfig{"os": {
		Kernel: app.KernelConfig{Path: "/opt/os-vmlinux", SHA256: digest64},
		Initrd: app.KernelConfig{Path: "/opt/os-initrd", SHA256: digest64},
	}}, cfg.Firecracker.BootSets)

	for _, tc := range []struct{ name, sets, reason string }{
		{"a bad name", strings.Replace(good, `"os"`, `"OS_1"`, 1), `boot_sets name "OS_1"`},
		{"a relative path", strings.Replace(good, `/opt/os-initrd`, `os-initrd`, 1), "initrd path"},
		{"an empty digest", strings.Replace(good, `"sha256":"`+digest64+`"}}}`, `"sha256":""}}}`, 1), "initrd sha256 is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.NotEqual(t, good, tc.sets)
			_, err := app.ParseConfig([]byte(withBootSets(tc.sets)))
			require.Error(t, err)
			assert.ErrorContains(t, err, tc.reason)
		})
	}
}

func TestBootSets_TheNodesCheckerNamesAFileThatChanged(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k")
	require.NoError(t, os.WriteFile(path, []byte("a kernel"), 0o600))
	sum := sha256.Sum256([]byte("a kernel"))
	file := machine.KernelFile{Path: path, SHA256: hex.EncodeToString(sum[:])}
	check := app.PinnedBootFiles()
	require.NoError(t, check.Check("the kernel of boot set os", file), "control: the file has its digest")

	require.NoError(t, os.WriteFile(path, []byte("another kernel"), 0o600))
	assert.ErrorContains(t, check.Check("the kernel of boot set os", file), "the kernel of boot set os")
}
