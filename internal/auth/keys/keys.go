// Package keys loads the cluster encryption key (docs/architecture.md §8.2,
// D57, D71): KUDURRU_ENCRYPTION_KEY[_FILE] and the rotation-window
// KUDURRU_ENCRYPTION_KEY_PREVIOUS[_FILE], base64 or hex (session.ParseKey).
// Single-instance Docker deployments that set neither get a key generated
// once and kept under <dataDir>/keys/; the Kubernetes driver refuses to
// start without one, because every replica must share it and a generated
// key would silently split sessions and the internal plane between pods.
package keys

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

// Environment variables.
const (
	EnvKey         = "KUDURRU_ENCRYPTION_KEY"
	EnvPreviousKey = "KUDURRU_ENCRYPTION_KEY_PREVIOUS"
)

// Dir and FileName locate the generated key: <dataDir>/keys/encryption.key.
const (
	Dir      = "keys"
	FileName = "encryption.key"
)

// ErrKeyRequired is returned in Kubernetes when no key is configured.
var ErrKeyRequired = errors.New("keys: " + EnvKey + " (or " + EnvKey + "_FILE) is required with the kube store driver: " +
	"every replica must share it (create a Secret with 32 random bytes, e.g. `openssl rand -base64 32`, and reference it in the chart)")

// Options configures Load.
type Options struct {
	// Getenv reads the environment; default os.Getenv.
	Getenv func(string) string
	// Driver is store.driver: "kube" refuses to generate a key.
	Driver string
	// DataDir is node.dataDir; the generated key lives in DataDir/keys.
	DataDir string
}

// Keys are the decoded encryption keys.
type Keys struct {
	// Current seals new sessions and derives the internal credentials.
	Current []byte
	// Previous opens what the previous key sealed (rotation window, D57).
	Previous []byte
	// Generated is true when Current was generated and written to Path by
	// this call (a key read back from Path is not generated)
	// rather than configured.
	Generated bool
	// Path is the generated key's file ("" when configured).
	Path string
}

// Load reads the keys from the environment, or in Docker mode reads or
// creates <DataDir>/keys/encryption.key (0600, directory 0700).
func Load(opts Options) (Keys, error) {
	if opts.Getenv == nil {
		opts.Getenv = os.Getenv
	}
	cur, err := readEnv(opts.Getenv, EnvKey)
	if err != nil {
		return Keys{}, err
	}
	prev, err := readEnv(opts.Getenv, EnvPreviousKey)
	if err != nil {
		return Keys{}, err
	}
	if cur != nil {
		return Keys{Current: cur, Previous: prev}, nil
	}
	if prev != nil {
		return Keys{}, fmt.Errorf("keys: %s is set without %s", EnvPreviousKey, EnvKey)
	}
	if opts.Driver == "kube" {
		return Keys{}, ErrKeyRequired
	}
	if opts.DataDir == "" {
		return Keys{}, errors.New("keys: no encryption key and no data directory to keep a generated one")
	}
	dir := filepath.Join(opts.DataDir, Dir)
	path := filepath.Join(dir, FileName)
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // G304: path is under node.dataDir
		k, err := session.ParseKey(strings.TrimSpace(string(b)))
		if err != nil {
			return Keys{}, fmt.Errorf("keys: %s: %w", path, err)
		}
		return Keys{Current: k, Generated: false, Path: path}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	tmp, err := os.CreateTemp(dir, ".encryption-*.key")
	if err != nil {
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	if _, err := tmp.WriteString(base64.StdEncoding.EncodeToString(k) + "\n"); err != nil {
		_ = tmp.Close()
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	// Link rather than rename: of two processes racing on first start the
	// loser keeps the winner's key instead of overwriting it.
	if err := os.Link(tmp.Name(), path); err != nil {
		if errors.Is(err, os.ErrExist) {
			return Load(opts)
		}
		return Keys{}, fmt.Errorf("keys: %w", err)
	}
	return Keys{Current: k, Generated: true, Path: path}, nil
}

func readEnv(getenv func(string) string, name string) ([]byte, error) {
	v := getenv(name)
	if f := getenv(name + "_FILE"); f != "" {
		b, err := os.ReadFile(f) //nolint:gosec // G304: the operator names the file
		if err != nil {
			return nil, fmt.Errorf("keys: %s_FILE: %w", name, err)
		}
		v = strings.TrimSpace(string(b))
	}
	if v == "" {
		return nil, nil
	}
	k, err := session.ParseKey(v)
	if err != nil {
		return nil, fmt.Errorf("keys: %s: %w", name, err)
	}
	return k, nil
}
