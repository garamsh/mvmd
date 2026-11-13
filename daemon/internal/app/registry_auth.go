package app

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/garamsh/mvmd/daemon/internal/ociregistry"
)

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

// sources is what the Registry adapter resolves the credentials and the CA
// from.
func (r RegistryConfig) sources() ociregistry.Sources {
	return ociregistry.Sources{CredentialsFile: r.CredentialsFile, CredentialHelpers: r.CredentialHelpers, CAFile: r.CAFile}
}
