package ociregistry

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// helperWaitDelay bounds how long a helper's descendants may hold its output
// open after the helper or the pull's context has ended.
const helperWaitDelay = 5 * time.Second

// Sources is where a node's registry credentials and CA come from, as the
// configuration names them. Any of the three may be empty.
type Sources struct {
	// CredentialsFile is the absolute path of a JSON file mapping a registry
	// to a username and a password.
	CredentialsFile string
	// CredentialHelpers maps a registry to the name of a docker credential
	// helper.
	CredentialHelpers map[string]string
	// CAFile is the absolute path of a PEM bundle added to the system roots.
	CAFile string
}

// NewFromSources checks every input at start and returns a Registry whose
// lookup and roots read the file, run the helper and read the CA at each call.
func NewFromSources(s Sources) (*Registry, error) {
	var opts Options
	if s.CredentialsFile != "" {
		if _, err := readCredentials(s.CredentialsFile, s.CredentialHelpers); err != nil {
			return nil, err
		}
	}
	if s.CAFile != "" {
		pool, err := readRoots(s.CAFile)
		if err != nil {
			return nil, err
		}
		opts.RootCAs = func(context.Context) (*x509.CertPool, error) { return pool, nil }
	}
	opts.Credentials = func(ctx context.Context, registry string) (Credential, error) {
		if name, ok := s.CredentialHelpers[registry]; ok {
			return runHelper(ctx, name, registry)
		}
		if s.CredentialsFile == "" {
			return Credential{}, nil
		}
		creds, err := readCredentials(s.CredentialsFile, s.CredentialHelpers)
		if err != nil {
			return Credential{}, err
		}
		return creds[registry], nil // an unlisted registry is anonymous
	}
	return New(opts), nil
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
func runHelper(ctx context.Context, name, registry string) (Credential, error) {
	cmd := exec.CommandContext(ctx, "docker-credential-"+name, "get")
	cmd.Stdin = strings.NewReader(registry)
	var out bytes.Buffer
	cmd.Stdout = &out // Stderr stays nil: discarded
	cmd.WaitDelay = helperWaitDelay
	if err := cmd.Run(); err != nil {
		return Credential{}, fmt.Errorf("registry.credential_helpers: helper %q for %q failed: %s", name, registry, helperFailure(ctx, err))
	}
	var got struct {
		Username string
		Secret   string
	}
	if err := json.Unmarshal(out.Bytes(), &got); err != nil || got.Username == "" || got.Secret == "" {
		return Credential{}, fmt.Errorf("registry.credential_helpers: helper %q for %q did not answer with a username and a secret", name, registry)
	}
	return Credential{Username: got.Username, Password: got.Secret}, nil
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

// readCredentials reads the credentials file and returns what it maps. A file
// that others can read, that does not parse, or that names a registry the
// node reaches through a helper is refused. No error names a username or a
// password.
func readCredentials(path string, helpers map[string]string) (map[string]Credential, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("registry.credentials_file: %w", err)
	}
	defer f.Close()
	info, err := f.Stat()
	switch {
	case err != nil:
		return nil, fmt.Errorf("registry.credentials_file: %w", err)
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("registry.credentials_file %q is not a regular file", path)
	case info.Mode().Perm()&0o077 != 0:
		return nil, fmt.Errorf("registry.credentials_file %q is accessible to group or others (mode %04o); it must be 0600 or closer", path, info.Mode().Perm())
	}
	var byRegistry map[string]struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&byRegistry); err != nil {
		// The decoding error is left out: it can quote a character of the file.
		return nil, fmt.Errorf("registry.credentials_file %q does not hold a JSON object of registries to username and password", path)
	}
	creds := make(map[string]Credential, len(byRegistry))
	for registry, c := range byRegistry {
		if _, ok := helpers[registry]; ok {
			return nil, fmt.Errorf("registry %q is named in both registry.credentials_file and registry.credential_helpers", registry)
		}
		if c.Username == "" || c.Password == "" {
			return nil, fmt.Errorf("registry.credentials_file: the entry of %q has no username or no password", registry)
		}
		creds[registry] = Credential{Username: c.Username, Password: c.Password}
	}
	return creds, nil
}
