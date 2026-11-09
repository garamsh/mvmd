package app_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
)

// The credentials file is read at start and refused unless it is closed to
// others and well formed, and the refusal never quotes what it holds.
func TestFirecracker_RegistryCredentialsAreCheckedAtStart(t *testing.T) {
	const secret = "hunter2-secret"
	good := `{"r.example":{"username":"u","password":"` + secret + `"}}`
	tests := []struct {
		name, content string
		mode          os.FileMode
		reason        string
	}{
		{"readable by the group", good, 0o640, "mode"},
		{"not JSON", secret, 0o600, "credentials"},
		{"an entry without a password", `{"r.example":{"username":"` + secret + `"}}`, 0o600, "password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := firecrackerConfig(t)
			path := filepath.Join(filepath.Dir(cfg.Socket.Path), "registry.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), tt.mode))
			require.NoError(t, os.Chmod(path, tt.mode))
			cfg.Registry = &app.RegistryConfig{CredentialsFile: path}

			_, err := app.New(cfg)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
			assert.NotContains(t, err.Error(), secret, "the error does not quote the file")

			// The control: a well formed file with the mode closed starts.
			require.NoError(t, os.WriteFile(path, []byte(good), 0o600))
			require.NoError(t, os.Chmod(path, 0o600))
			_, err = app.New(cfg)
			require.NoError(t, err)
		})
	}

	t.Run("a missing file", func(t *testing.T) {
		cfg := firecrackerConfig(t)
		cfg.Registry = &app.RegistryConfig{CredentialsFile: filepath.Join(t.TempDir(), "absent.json")}
		_, err := app.New(cfg)
		assert.Error(t, err)
	})
}
