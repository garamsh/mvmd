package app

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
)

// helperWaitDelay bounds how long a helper's descendants may hold its output
// open after the helper or the pull's context has ended.
const helperWaitDelay = 5 * time.Second

// validate checks the keys of the section that need no file.
func (r RegistryConfig) validate() error {
	if r.CredentialsFile == "" && len(r.CredentialHelpers) == 0 && r.CAFile == "" {
		return errors.New("the registry section sets none of registry.credentials_file, registry.credential_helpers and registry.ca_file")
	}
	if r.CredentialsFile != "" && !filepath.IsAbs(r.CredentialsFile) {
		return fmt.Errorf("registry.credentials_file %q is not an absolute path", r.CredentialsFile)
	}
	if r.CAFile != "" && !filepath.IsAbs(r.CAFile) {
		return fmt.Errorf("registry.ca_file %q is not an absolute path", r.CAFile)
	}
	for registry, name := range r.CredentialHelpers {
		if registry == "" || name == "" || strings.ContainsAny(name, "/\x00") {
			return fmt.Errorf("registry.credential_helpers: the entry of %q needs a registry and a helper name with no slash", registry)
		}
	}
	return nil
}

// options checks every input at start and sets the lookup and the roots of the
// Registry adapter, which read the file, run the helper and read the CA at
// each call.
func (r RegistryConfig) options(opts *ociregistry.Options) error {
	if r.CredentialsFile != "" {
		if _, err := readCredentials(r.CredentialsFile, r.CredentialHelpers); err != nil {
			return err
		}
	}
	if r.CAFile != "" {
		if _, err := readRoots(r.CAFile); err != nil {
			return err
		}
		opts.RootCAs = func(context.Context) (*x509.CertPool, error) { return readRoots(r.CAFile) }
	}
	opts.Credentials = func(ctx context.Context, registry string) (ociregistry.Credential, error) {
		if name, ok := r.CredentialHelpers[registry]; ok {
			return runHelper(ctx, name, registry)
		}
		if r.CredentialsFile == "" {
			return ociregistry.Credential{}, nil
		}
		creds, err := readCredentials(r.CredentialsFile, r.CredentialHelpers)
		if err != nil {
			return ociregistry.Credential{}, err
		}
		return creds[registry], nil // an unlisted registry is anonymous
	}
	return nil
}

// readRoots returns the system roots with the certificates of the PEM bundle
// at path added.
func readRoots(path string) (*x509.CertPool, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry.ca_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("registry.ca_file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("registry.ca_file %q is not a regular file", path)
	}
	var bundle bytes.Buffer
	if _, err := bundle.ReadFrom(f); err != nil {
		return nil, fmt.Errorf("registry.ca_file: %w", err)
	}
	pool, err := x509.SystemCertPool()
	if err != nil {
		pool = x509.NewCertPool()
	}
	if !pool.AppendCertsFromPEM(bundle.Bytes()) {
		return nil, fmt.Errorf("registry.ca_file %q holds no certificate", path)
	}
	return pool, nil
}

// runHelper runs docker-credential-<name> get for the registry, with mvmd's
// environment. Neither its output nor its error text reaches the returned
// error, since either can carry the secret.
func runHelper(ctx context.Context, name, registry string) (ociregistry.Credential, error) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(registry)
	var out bytes.Buffer
	cmd.Stdout = &out // Stderr stays nil: discarded
	cmd.WaitDelay = helperWaitDelay
	if err := cmd.Run(); err != nil {
		return ociregistry.Credential{}, fmt.Errorf("registry.credential_helpers: helper %q for %q failed: %s", name, registry, helperFailure(ctx, err))
	}
	var got struct {
		Username string
		Secret   string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Username == "" || got.Secret == "" {
		return ociregistry.Credential{}, fmt.Errorf("registry.credential_helpers: helper %q for %q did not answer with a username and a secret", name, registry)
	}
	return ociregistry.Credential{Username: got.Username, Password: got.Secret}, nil
}

// helperFailure says why a helper did not run to success, without anything it
// wrote.
func helperFailure(ctx context.Context, err error) string {
	var exit *exec.ExitError
	switch {
	case ctx.Err() != nil:
		return ctx.Err().Error()
	case errors.Is(err, exec.ErrNotFound):
		return "not found on PATH"
	case errors.As(err, &exit):
		return exit.ProcessState.String()
	default:
		return "did not run"
	}
}
