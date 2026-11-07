package dnsserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// AcquisitionKeys retains one original immutable credential installation for a
// complete background acquisition. Current is memory-only. WithCurrent pins
// that installation, checks the original source, runs local cache work, checks
// source/time again, then invokes optional memory-only publication. It cannot
// refuse a source postcheck after publication or turn a local cache write into
// an authoritative acknowledgment. A failed local artifact stays provenance.
// Callers enter this pin before manager publication or local persistence gates.
type AcquisitionKeys interface {
	Keys() *StaticKeyring
	Identity() string
	Current() bool
	WithCurrent(context.Context, func() error, func() error) error
}

// AcquisitionFingerprint identifies data acquired with one credential. It is
// provenance only, never current command authority. Immutable stored inputs
// include their explicit epoch; legacy file inputs retain their durable hash.
// Captured production jobs pass a StaticKeyring from their original key cut.
func AcquisitionFingerprint(keys TSIGKeyring, name string) (string, bool) {
	if keys == nil {
		return "", false
	}
	name = TSIGKeyName(name)
	secret, algorithm, ok := keys.Lookup(name)
	if !ok || len(secret) == 0 {
		return "", false
	}
	if _, ok := TSIGAlgorithm(algorithm); !ok {
		return "", false
	}
	if immutable, ok := keys.(*StaticKeyring); ok && immutable.keys[name].CredentialEpoch != "" {
		identity, ok := immutable.Identity(name)
		if !ok {
			return "", false
		}
		sum := sha256.Sum256([]byte("kudurru/stored-tsig-acquisition/v1/" + identity.CredentialVersion))
		return "stored/v1/" + hex.EncodeToString(sum[:]), true
	}
	// This exact legacy digest is already persisted by secondary/RPZ. Changing
	// it would unnecessarily invalidate unexpired file-backed copies on restart.
	sum := sha256.Sum256(append(append([]byte(algorithm), 0), secret...))
	return hex.EncodeToString(sum[:]), true
}
