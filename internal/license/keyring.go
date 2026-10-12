package license

import (
	"crypto/ed25519"
	"embed"
	"encoding/base64"
	"fmt"
	"io/fs"
	"path"
	"strings"
)

// keyFS holds the trusted licensor public keys: one file per key, named
// <kid>.pub, containing the raw 32-byte Ed25519 key in base64. Add a file to
// trust a new signing key (rotation); keep the old one while keys it signed
// are in the field.
//
//go:embed keys/*.pub
var keyFS embed.FS

// testKeys are extra trusted keys; only a build with -tags licensetest sets
// them (testkey.go).
var testKeys Keyring

// EmbeddedKeyring returns the keyring compiled into the binary.
func EmbeddedKeyring() (Keyring, error) {
	ring, err := keyringFrom(keyFS, "keys")
	if err != nil {
		return nil, err
	}
	for kid, pub := range testKeys {
		ring[kid] = pub
	}
	return ring, nil
}

func keyringFrom(fsys fs.FS, dir string) (Keyring, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, err
	}
	ring := Keyring{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".pub") {
			continue
		}
		b, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, err
		}
		pub, err := ParsePublicKey(string(b))
		if err != nil {
			return nil, fmt.Errorf("license key %s: %w", e.Name(), err)
		}
		ring[strings.TrimSuffix(e.Name(), ".pub")] = pub
	}
	if len(ring) == 0 {
		return nil, fmt.Errorf("no license public keys in %s", dir)
	}
	return ring, nil
}

// ParsePublicKey decodes the base64 form of an Ed25519 public key.
func ParsePublicKey(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(clean(s))
	if err != nil {
		return nil, fmt.Errorf("not base64: %w", err)
	}
	if len(b) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("public key is %d bytes, want %d", len(b), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(b), nil
}

// EncodePublicKey is the inverse of ParsePublicKey.
func EncodePublicKey(pub ed25519.PublicKey) string {
	return base64.StdEncoding.EncodeToString(pub)
}
