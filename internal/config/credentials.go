package config

import (
	"fmt"
	"path/filepath"
	"regexp"
)

// CredentialGrants are host capabilities delegated to stored provider objects.
// They contain references only, never credential values, and are not replicated.
type CredentialGrants struct {
	AllowedFiles       []string `koanf:"allowedFiles" json:"allowedFiles"`
	AllowedEnvironment []string `koanf:"allowedEnvironment" json:"allowedEnvironment"`
}

// CredentialRef names one credential for a cluster setting without holding
// its value: a key of a Secret in the namespace the server runs in
// (Kubernetes), a file or an environment variable. It has the shape of the
// CredentialReference of stored objects (api/v1alpha1) and the same rule:
// a file or environment reference works only on a node whose
// credentials.allowedFiles or credentials.allowedEnvironment names it, so a
// settings change cannot make a node read anything its operator did not
// delegate.
type CredentialRef struct {
	SecretRef *CredentialSecretKey `koanf:"secretRef" json:"secretRef,omitempty"`
	File      string               `koanf:"file" json:"file,omitempty"`
	Env       string               `koanf:"env" json:"env,omitempty"`
}

// CredentialSecretKey is one key of a Secret.
type CredentialSecretKey struct {
	Name string `koanf:"name" json:"name"`
	Key  string `koanf:"key" json:"key"`
}

var credentialEnvironmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func (c CredentialGrants) validate() []error {
	var errs []error
	if len(c.AllowedFiles) > 256 || len(c.AllowedEnvironment) > 256 {
		errs = append(errs, fmt.Errorf("credentials: at most 256 file and environment grants are allowed"))
	}
	for _, path := range c.AllowedFiles {
		if len(path) > 4096 || !filepath.IsAbs(path) || filepath.Clean(path) != path {
			errs = append(errs, fmt.Errorf("credentials.allowedFiles requires clean absolute paths of at most 4096 bytes"))
			break
		}
	}
	for _, name := range c.AllowedEnvironment {
		if len(name) > 128 || !credentialEnvironmentName.MatchString(name) {
			errs = append(errs, fmt.Errorf("credentials.allowedEnvironment requires valid names of at most 128 bytes"))
			break
		}
	}
	return errs
}
