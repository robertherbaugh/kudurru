// Package session implements the sealed SessionStore of docs/architecture.md
// §8.2 (decisions D23, D48, D54, D57) and the cookie-authenticating
// middleware of §8.8.
//
// A sealed session is an AES-256-GCM encrypted, authenticated cookie whose
// payload is {sid, principal, roles_snapshot, idp, iat, exp, idle_exp, csrf}.
// Any replica holding the cluster-shared KUDURRU_ENCRYPTION_KEY can open it,
// which is why a Kubernetes Deployment needs no shared session database.
// The price is that the store cannot list or revoke individual sessions;
// revocation goes through the sessionsValidAfter hook instead.
package session

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of KUDURRU_ENCRYPTION_KEY in bytes (AES-256).
const KeySize = 32

// Purposes separate the keys derived from the one master key, so a value
// sealed for one use can never be opened as another (a pre-auth cookie
// replayed as a session cookie, for example).
const (
	PurposeSession     = "session"
	PurposeOIDCPreAuth = "oidc-preauth"
)

// envelopeV1 is the first byte of every sealed value. Bumping it lets a
// future format coexist with cookies already in browsers.
const envelopeV1 byte = 1

const (
	keyIDLen  = 4
	headerLen = 1 + keyIDLen
	nonceLen  = 12 // GCM standard nonce, generated randomly per seal
	tagLen    = 16
	// minSealed is the shortest possible valid envelope (empty plaintext).
	minSealed = headerLen + nonceLen + tagLen
)

// ErrInvalid is returned for any value that does not open: wrong encoding,
// unknown envelope version, unknown key, truncated, or tampered. Callers
// must not distinguish these cases to the client.
var ErrInvalid = errors.New("session: invalid or tampered sealed value")

type sealKey struct {
	id   [keyIDLen]byte
	aead cipher.AEAD
}

// Sealer encrypts and authenticates small payloads into URL-safe strings
// with AES-256-GCM and a random 96-bit nonce. It seals with the current key
// and opens with the current or the previous key, which is the D57 dual-key
// rotation window (KUDURRU_ENCRYPTION_KEY_PREVIOUS).
//
// Keys are derived from the master key with HKDF-SHA256 using the purpose
// as the info string, so the same master key can safely serve sessions,
// pre-auth cookies and (later) stored secrets.
//
// A Sealer is safe for concurrent use.
type Sealer struct {
	purpose  string
	current  sealKey
	previous *sealKey
}

// NewSealer builds a Sealer for purpose from the 32-byte master key and an
// optional 32-byte previous master key (nil or empty when not rotating).
func NewSealer(purpose string, current, previous []byte) (*Sealer, error) {
	if purpose == "" {
		return nil, errors.New("session: sealer purpose is required")
	}
	cur, err := deriveKey(purpose, current)
	if err != nil {
		return nil, fmt.Errorf("session: current key: %w", err)
	}
	s := &Sealer{purpose: purpose, current: cur}
	if len(previous) > 0 {
		prev, err := deriveKey(purpose, previous)
		if err != nil {
			return nil, fmt.Errorf("session: previous key: %w", err)
		}
		if prev.id == cur.id {
			return nil, errors.New("session: previous key equals current key")
		}
		s.previous = &prev
	}
	return s, nil
}

func deriveKey(purpose string, master []byte) (sealKey, error) {
	if len(master) != KeySize {
		return sealKey{}, fmt.Errorf("must be %d bytes, got %d", KeySize, len(master))
	}
	k, err := hkdf.Key(sha256.New, master, nil, "kudurru/"+purpose+"/v1", KeySize)
	if err != nil {
		return sealKey{}, err
	}
	block, err := aes.NewCipher(k)
	if err != nil {
		return sealKey{}, err
	}
	aead, err := cipher.NewGCMWithRandomNonce(block)
	if err != nil {
		return sealKey{}, err
	}
	// The key id lets Open pick the right key without trial decryption and
	// lets logs say "sealed under an unknown key" after a rotation. Four
	// bytes of a hash of a 256-bit derived key reveal nothing useful.
	sum := sha256.Sum256(append([]byte("kudurru key id\x00"), k...))
	sk := sealKey{aead: aead}
	copy(sk.id[:], sum[:keyIDLen])
	return sk, nil
}

// Seal encrypts plaintext under the current key and returns
// base64url(version | key id | nonce | ciphertext | tag) without padding.
// The header is authenticated as additional data.
func (s *Sealer) Seal(plaintext []byte) string {
	buf := make([]byte, headerLen, headerLen+nonceLen+len(plaintext)+tagLen)
	buf[0] = envelopeV1
	copy(buf[1:headerLen], s.current.id[:])
	// The random-nonce AEAD prepends its nonce to the ciphertext.
	out := s.current.aead.Seal(buf, nil, plaintext, buf[:headerLen]) //nolint:gosec // G407: NewGCMWithRandomNonce draws the nonce itself and requires nil here
	return base64.RawURLEncoding.EncodeToString(out)
}

// Open authenticates and decrypts a value produced by Seal under the
// current or the previous key. usedPrevious reports that the value was
// sealed under the previous key, so callers can re-seal it under the
// current one.
func (s *Sealer) Open(value string) (plaintext []byte, usedPrevious bool, err error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(raw) < minSealed || raw[0] != envelopeV1 {
		return nil, false, ErrInvalid
	}
	var id [keyIDLen]byte
	copy(id[:], raw[1:headerLen])
	key := &s.current
	switch {
	case id == s.current.id:
	case s.previous != nil && id == s.previous.id:
		key, usedPrevious = s.previous, true
	default:
		return nil, false, ErrInvalid
	}
	pt, err := key.aead.Open(nil, nil, raw[headerLen:], raw[:headerLen])
	if err != nil {
		return nil, false, ErrInvalid
	}
	return pt, usedPrevious, nil
}

// Overhead is the number of bytes Seal adds before base64 encoding.
const Overhead = headerLen + nonceLen + tagLen

// SealedLen returns the length of Seal's output for a plaintext of n bytes.
func SealedLen(n int) int { return base64.RawURLEncoding.EncodedLen(n + Overhead) }

// ParseKey decodes a 32-byte key given as standard or URL-safe base64
// (padded or not) or as 64 hex digits, the forms accepted for
// KUDURRU_ENCRYPTION_KEY and KUDURRU_ENCRYPTION_KEY_PREVIOUS. Surrounding
// whitespace (a trailing newline in a *_FILE) is ignored.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if len(s) == hex.EncodedLen(KeySize) {
		if k, err := hex.DecodeString(s); err == nil {
			return k, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if k, err := enc.DecodeString(s); err == nil {
			if len(k) != KeySize {
				return nil, fmt.Errorf("session: key decodes to %d bytes, want %d", len(k), KeySize)
			}
			return k, nil
		}
	}
	return nil, errors.New("session: key is neither base64 nor 64 hex digits")
}
