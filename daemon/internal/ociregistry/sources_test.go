package ociregistry_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
)

const authRepo = "library/app"

// authRegistry is a TLS registry that serves one manifest to the identity
// accepts says, and asks for basic credentials of any other; a nil accepts
// serves anonymously. A test that needs the registry's host in the identity, or
// a registry that stops accepting a credential it accepted, calls accept after
// construction.
type authRegistry struct {
	srv *httptest.Server
	ref image.Reference

	mu      sync.Mutex
	accepts func(user, password string) bool
}

// accept replaces the identity the registry serves; nil serves anonymously.
func (r *authRegistry) accept(accepts func(user, password string) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.accepts = accepts
}

func (r *authRegistry) allows(req *http.Request) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	u, p, ok := req.BasicAuth()
	return r.accepts == nil || (ok && r.accepts(u, p))
}

func newAuthRegistry(t *testing.T, accepts func(user, password string) bool) *authRegistry {
	t.Helper()
	body := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:` +
		hex.EncodeToString(sum("{}")) + `","size":2},"layers":[]}`)
	d := "sha256:" + hex.EncodeToString(sum(string(body)))
	r := &authRegistry{accepts: accepts}
	r.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !r.allows(req) {
			w.Header().Set("WWW-Authenticate", `Basic realm="fake"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
		w.Write(body)
	}))
	t.Cleanup(r.srv.Close)
	r.ref = image.Reference{Repository: r.host() + "/" + authRepo, Digest: machine.ImageDigest(d)}
	return r
}

func sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

func (r *authRegistry) host() string { return r.srv.Listener.Addr().String() }

// caFile writes the registry's certificate as the PEM bundle a node trusts.
func (r *authRegistry) caFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: r.srv.Certificate().Raw}), 0o644))
	return path
}

var platformAMD64 = image.Platform{OS: "linux", Architecture: "amd64"}

// pull fetches the manifest through a Registry built as the node builds it.
func pull(t *testing.T, cfg ociregistry.Sources, ref image.Reference) error {
	t.Helper()
	registry, err := ociregistry.NewFromSources(cfg)
	require.NoError(t, err)
	_, err = registry.Manifest(context.Background(), ref, platformAMD64)
	return err
}

func writeCredentials(t *testing.T, path, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, os.Chmod(path, 0o600))
}

// newRegistry builds one Registry as the node builds it, and returns the pull
// through it.
func newRegistry(t *testing.T, cfg ociregistry.Sources, ref image.Reference) func() error {
	t.Helper()
	registry, err := ociregistry.NewFromSources(cfg)
	require.NoError(t, err)
	return func() error {
		_, err := registry.Manifest(context.Background(), ref, platformAMD64)
		return err
	}
}

func acceptsPassword(password string) func(u, p string) bool {
	return func(u, p string) bool { return u == "node" && p == password }
}

// A pull after the credentials file is rewritten uses the new credential, with
// the same Registry and no restart.
func TestRegistryAuth_ARewrittenCredentialsFileRenewsTheNextPull(t *testing.T) {
	const first, second = "first-password", "second-password"
	reg := newAuthRegistry(t, acceptsPassword(second))
	file := filepath.Join(t.TempDir(), "registry.json")
	writeCredentials(t, file, fmt.Sprintf(`{%q:{"username":"node","password":%q}}`, reg.host(), first))
	manifest := newRegistry(t, ociregistry.Sources{CredentialsFile: file, CAFile: reg.caFile(t)}, reg.ref)

	err := manifest()
	require.Error(t, err, "control: the first credential is refused as unauthorized")
	assert.NotContains(t, err.Error(), first)

	writeCredentials(t, file, fmt.Sprintf(`{%q:{"username":"node","password":%q}}`, reg.host(), second))
	require.NoError(t, manifest())
}

// A credential from the file that worked and then expired is replaced by the
// file's new one on the next pull, with the same Registry.
func TestRegistryAuth_ACredentialsFileCredentialThatExpiredIsRenewed(t *testing.T) {
	const first, second = "first-password", "second-password"
	reg := newAuthRegistry(t, acceptsPassword(first))
	file := filepath.Join(t.TempDir(), "registry.json")
	writeCredentials(t, file, fmt.Sprintf(`{%q:{"username":"node","password":%q}}`, reg.host(), first))
	manifest := newRegistry(t, ociregistry.Sources{CredentialsFile: file, CAFile: reg.caFile(t)}, reg.ref)
	require.NoError(t, manifest(), "control: the first credential is accepted")

	reg.accept(acceptsPassword(second))
	require.Error(t, manifest(), "control: the registry no longer accepts the first credential, and the file still holds it")

	writeCredentials(t, file, fmt.Sprintf(`{%q:{"username":"node","password":%q}}`, reg.host(), second))
	require.NoError(t, manifest())
}

// A registry served with a certificate from a private CA is pulled from once
// ca_file names that CA.
func TestRegistryAuth_ACAFileIsTrustedForTheRegistryTLS(t *testing.T) {
	reg := newAuthRegistry(t, nil)

	err := pull(t, ociregistry.Sources{CredentialHelpers: map[string]string{"other.example": "x"}}, reg.ref)
	require.Error(t, err, "control: without ca_file the certificate fails verification")
	assert.Contains(t, err.Error(), "certificate")

	require.NoError(t, pull(t, ociregistry.Sources{CAFile: reg.caFile(t)}, reg.ref))
}

// A ca_file that is not a regular file or holds no certificate is refused at
// start.
func TestRegistryAuth_ACAFileWithoutACertificateIsRefusedAtStart(t *testing.T) {
	reg := newAuthRegistry(t, nil)
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	require.NoError(t, os.WriteFile(empty, []byte("not a certificate"), 0o644))

	for name, path := range map[string]string{"no certificate": empty, "a directory": dir, "absent": filepath.Join(dir, "absent.pem")} {
		_, err := ociregistry.NewFromSources(ociregistry.Sources{CAFile: path})
		assert.Error(t, err, name)
	}
	_, err := ociregistry.NewFromSources(ociregistry.Sources{CAFile: reg.caFile(t)})
	assert.NoError(t, err, "control: a bundle with a certificate starts")
}

// helperDir puts fake docker-credential-fake, docker-credential-env and
// docker-credential-file on PATH. The first answers with a password derived
// from the server URL it reads, or writes a marker to both outputs and fails
// while MVMD_TEST_HELPER_FAIL is set. The second answers with the variable
// MVMD_TEST_HELPER_ENV as the username. The third answers with the first line
// of the file MVMD_TEST_HELPER_SECRET_FILE names as the secret, so a test
// changes its answer by rewriting the file.
func helperDir(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	scripts := map[string]string{
		"docker-credential-fake": `#!/bin/sh
read -r host
if [ -n "$MVMD_TEST_HELPER_FAIL" ]; then echo leaked-secret; echo leaked-secret >&2; exit 1; fi
printf '{"ServerURL":"%s","Username":"node","Secret":"pw-for-%s"}' "$host" "$host"
`,
		"docker-credential-env": `#!/bin/sh
printf '{"ServerURL":"x","Username":"%s","Secret":"s"}' "$MVMD_TEST_HELPER_ENV"
`,
		"docker-credential-file": `#!/bin/sh
read -r host
read -r secret < "$MVMD_TEST_HELPER_SECRET_FILE"
printf '{"ServerURL":"%s","Username":"node","Secret":"%s"}' "$host" "$secret"
`,
	}
	for name, script := range scripts {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755))
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// A pull from a host with a helper runs the helper, which reads the host, and
// uses what it returns.
func TestRegistryAuth_AHelperIsRunAndItsAnswerIsUsed(t *testing.T) {
	helperDir(t)
	reg := newAuthRegistry(t, nil)
	reg.accept(func(u, p string) bool { return u == "node" && p == "pw-for-"+reg.host() })
	ca := reg.caFile(t)

	require.Error(t, pull(t, ociregistry.Sources{CAFile: ca}, reg.ref), "control: without the helper the registry refuses")
	require.NoError(t, pull(t, ociregistry.Sources{CAFile: ca, CredentialHelpers: map[string]string{reg.host(): "fake"}}, reg.ref))
}

// A helper's credential that worked and then expired is replaced by the
// helper's new answer on the next pull, with the same Registry.
func TestRegistryAuth_AHelperCredentialThatExpiredIsRenewed(t *testing.T) {
	const first, second = "first-secret", "second-secret"
	helperDir(t)
	answer := filepath.Join(t.TempDir(), "answer")
	require.NoError(t, os.WriteFile(answer, []byte(first+"\n"), 0o600))
	t.Setenv("MVMD_TEST_HELPER_SECRET_FILE", answer)
	reg := newAuthRegistry(t, acceptsPassword(first))
	manifest := newRegistry(t, ociregistry.Sources{CAFile: reg.caFile(t), CredentialHelpers: map[string]string{reg.host(): "file"}}, reg.ref)
	require.NoError(t, manifest(), "control: the helper's first answer is accepted")

	reg.accept(acceptsPassword(second))
	require.Error(t, manifest(), "control: the registry no longer accepts the first answer, and the helper still gives it")

	require.NoError(t, os.WriteFile(answer, []byte(second+"\n"), 0o600))
	require.NoError(t, manifest())
}

// A helper that exits non-zero fails the pull, and the error names the helper
// and carries nothing it wrote.
func TestRegistryAuth_AFailingHelperFailsThePullWithoutItsOutput(t *testing.T) {
	helperDir(t)
	reg := newAuthRegistry(t, nil)
	reg.accept(func(u, p string) bool { return u == "node" && p == "pw-for-"+reg.host() })
	cfg := ociregistry.Sources{CAFile: reg.caFile(t), CredentialHelpers: map[string]string{reg.host(): "fake"}}
	require.NoError(t, pull(t, cfg, reg.ref), "control: the same helper succeeds")

	t.Setenv("MVMD_TEST_HELPER_FAIL", "1")
	err := pull(t, cfg, reg.ref)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"fake"`)
	assert.NotContains(t, err.Error(), "leaked-secret", "the helper's stdout and stderr are not in the error")
	assert.NotContains(t, err.Error(), "pw-for", "the secret is not in the error")
}

// A variable set in mvmd's environment reaches the helper.
func TestRegistryAuth_AHelperRunsWithMvmdsEnvironment(t *testing.T) {
	helperDir(t)
	reg := newAuthRegistry(t, func(u, p string) bool { return u == "from-mvmd" && p == "s" })
	cfg := ociregistry.Sources{CAFile: reg.caFile(t), CredentialHelpers: map[string]string{reg.host(): "env"}}

	t.Setenv("MVMD_TEST_HELPER_ENV", "other")
	require.Error(t, pull(t, cfg, reg.ref), "control: the helper answers with the variable, and the registry refuses another value")

	t.Setenv("MVMD_TEST_HELPER_ENV", "from-mvmd")
	require.NoError(t, pull(t, cfg, reg.ref))
}

// A registry named in both the credentials file and the helpers is refused at
// start, naming it.
func TestRegistryAuth_ARegistryInBothMapsIsRefusedAtStart(t *testing.T) {
	file := filepath.Join(t.TempDir(), "registry.json")
	writeCredentials(t, file, `{"a.example":{"username":"u","password":"p"},"b.example":{"username":"u","password":"p"}}`)

	_, err := ociregistry.NewFromSources(ociregistry.Sources{CredentialsFile: file, CredentialHelpers: map[string]string{"b.example": "fake"}})
	require.Error(t, err)
	assert.Contains(t, err.Error(), `"b.example"`)
	assert.NotContains(t, err.Error(), `"p"`)

	_, err = ociregistry.NewFromSources(ociregistry.Sources{CredentialsFile: file, CredentialHelpers: map[string]string{"c.example": "fake"}})
	assert.NoError(t, err, "control: disjoint maps start")
}

// A credentials file that others can read, that is not JSON or that has an
// entry without a password is refused at start, and the error quotes nothing
// of it.
func TestRegistryAuth_ACredentialsFileThatIsNotWellFormedIsRefusedAtStart(t *testing.T) {
	const secret = "hunter2-secret"
	good := `{"r.example":{"username":"u","password":"` + secret + `"}}`
	tests := []struct {
		name, content string
		mode          os.FileMode
		reason        string
	}{
		{"readable by the group", good, 0o640, "mode"},
		{"not JSON", secret, 0o600, "username and password"},
		{"an entry without a password", `{"r.example":{"username":"` + secret + `"}}`, 0o600, "no password"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "registry.json")
			require.NoError(t, os.WriteFile(path, []byte(tt.content), tt.mode))
			require.NoError(t, os.Chmod(path, tt.mode))

			_, err := ociregistry.NewFromSources(ociregistry.Sources{CredentialsFile: path})
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.reason)
			assert.NotContains(t, err.Error(), secret)

			writeCredentials(t, path, good)
			_, err = ociregistry.NewFromSources(ociregistry.Sources{CredentialsFile: path})
			assert.NoError(t, err, "control: a well formed file with the mode closed starts")
		})
	}
}
