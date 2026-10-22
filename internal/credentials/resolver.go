// Package credentials resolves explicitly delegated host capabilities for
// background integrations. It is never used on the DNS query path.
package credentials

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxBytes bounds each resolved credential in memory.
const MaxBytes = 64 << 10

var (
	// ErrDenied means a reference requests a capability not delegated by the host.
	ErrDenied = errors.New("credential reference is not allowed")
	// ErrUnavailable means an allowed credential cannot currently be loaded.
	ErrUnavailable = errors.New("credential is unavailable")
)

// resolutionError preserves retry classification without exposing provider
// response bodies through ordinary error formatting or diagnostics.
type resolutionError struct{ cause error }

func (resolutionError) Error() string      { return ErrUnavailable.Error() }
func (e resolutionError) GoString() string { return e.Error() }
func (e resolutionError) Unwrap() []error  { return []error{ErrUnavailable, e.cause} }

// Options is trusted node configuration. Public objects may select only these
// capabilities; they cannot extend the namespace, path or environment lists.
type Options struct {
	Files       []string
	Environment []string
	Namespace   string
	Secret      func(context.Context, string, string, string) ([]byte, error)
	Getenv      func(string) (string, bool)
	// FingerprintKey is at least 32 random bytes from the cluster keyring. A
	// keyed fingerprint avoids publishing an offline verifier for weak secrets.
	FingerprintKey []byte
}

// Resolver holds one immutable set of host credential capabilities.
type Resolver struct {
	files, environment map[string]bool
	namespace          string
	secret             func(context.Context, string, string, string) ([]byte, error)
	getenv             func(string) (string, bool)
	key                []byte
}

// Value belongs to the caller. Fingerprint is safe for status and change
// detection; Bytes must never be placed in spec, status, logs or audit events.
type Value struct {
	Bytes       []byte
	Fingerprint string
}

// New validates and captures the operator's credential capabilities.
func New(opts Options) (*Resolver, error) {
	if len(opts.FingerprintKey) < 32 {
		return nil, errors.New("credentials: fingerprint key needs at least 32 bytes")
	}
	r := &Resolver{files: map[string]bool{}, environment: map[string]bool{}, namespace: opts.Namespace, secret: opts.Secret, getenv: opts.Getenv, key: slices.Clone(opts.FingerprintKey)}
	if r.getenv == nil {
		r.getenv = os.LookupEnv
	}
	if r.secret != nil && len(validation.IsDNS1123Label(r.namespace)) != 0 {
		return nil, errors.New("credentials: secret namespace must be a DNS label")
	}
	for _, name := range opts.Files {
		if !filepath.IsAbs(name) || filepath.Clean(name) != name {
			return nil, errors.New("credentials: allowed files must use clean absolute paths")
		}
		r.files[name] = true
	}
	for _, name := range opts.Environment {
		if len(validation.IsEnvVarName(name)) != 0 {
			return nil, errors.New("credentials: invalid allowed environment name")
		}
		r.environment[name] = true
	}
	return r, nil
}

// Resolve does bounded background I/O and returns a private copy. References
// are rechecked on each call, so credentials cannot escape their captured host
// capabilities through an object edit. Construct a new Resolver on host reload.
func (r *Resolver) Resolve(ctx context.Context, ref v1alpha1.CredentialReference) (Value, error) {
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	count := 0
	for _, set := range []bool{ref.SecretRef != nil, ref.File != "", ref.Env != ""} {
		if set {
			count++
		}
	}
	if count != 1 {
		return Value{}, ErrDenied
	}
	var data []byte
	var identity string
	switch {
	case ref.SecretRef != nil:
		s := ref.SecretRef
		if r.secret == nil || len(validation.IsDNS1123Subdomain(s.Name)) != 0 || len(validation.IsConfigMapKey(s.Key)) != 0 || s.Key == "" {
			return Value{}, ErrDenied
		}
		var err error
		data, err = r.secret(ctx, r.namespace, s.Name, s.Key)
		if err != nil {
			return Value{}, resolutionError{cause: err}
		}
		identity = "secret:" + r.namespace + "/" + s.Name + "/" + s.Key
	case ref.File != "":
		if !r.files[ref.File] {
			return Value{}, ErrDenied
		}
		// The operator owns this exact path, including symlink replacement used
		// by mounted secrets. A public reference cannot select a sibling path.
		f, err := os.OpenFile(ref.File, os.O_RDONLY|syscall.O_NONBLOCK, 0) //nolint:gosec // checked against host allowlist above
		if err != nil {
			return Value{}, ErrUnavailable
		}
		defer func() { _ = f.Close() }()
		st, err := f.Stat()
		if err != nil || !st.Mode().IsRegular() || st.Size() > MaxBytes {
			return Value{}, ErrUnavailable
		}
		data, err = io.ReadAll(io.LimitReader(f, MaxBytes+1))
		if err != nil {
			return Value{}, ErrUnavailable
		}
		identity = "file:" + ref.File
	case ref.Env != "":
		if !r.environment[ref.Env] {
			return Value{}, ErrDenied
		}
		value, exists := r.getenv(ref.Env)
		if !exists {
			return Value{}, ErrUnavailable
		}
		data, identity = []byte(value), "env:"+ref.Env
	}
	if err := ctx.Err(); err != nil {
		return Value{}, err
	}
	if len(data) == 0 || len(data) > MaxBytes || strings.TrimSpace(string(data)) == "" {
		return Value{}, ErrUnavailable
	}
	h := hmac.New(sha256.New, r.key)
	_, _ = h.Write([]byte(identity))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write(data)
	return Value{Bytes: slices.Clone(data), Fingerprint: "hmac-sha256:" + hex.EncodeToString(h.Sum(nil))}, nil
}
