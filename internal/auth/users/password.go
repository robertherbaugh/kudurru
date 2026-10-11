package users

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters of §8.7 (the OWASP minimum: m=19 MiB, t=2, p=1).
const (
	ArgonMemoryKiB = 19 * 1024
	ArgonTime      = 2
	ArgonThreads   = 1
	argonSaltLen   = 16
	argonKeyLen    = 32
)

// Password length limits. The cap bounds the work an attacker can force
// per request; the minimum applies when a password is set, never at login.
const (
	MaxPasswordLen = 1024
	MinPasswordLen = 8
)

// Errors of the password functions.
var (
	ErrPasswordTooLong  = fmt.Errorf("users: password exceeds %d bytes", MaxPasswordLen)
	ErrPasswordTooShort = fmt.Errorf("users: password must have at least %d characters", MinPasswordLen)
	ErrBadHash          = errors.New("users: malformed Argon2id PHC string")
)

// Hasher computes and verifies Argon2id PHC strings. Concurrent hashes are
// bounded by a semaphore (§8.7 "global hashing semaphore"): every hash
// allocates 19 MiB, so a login flood must queue instead of exhausting
// memory.
type Hasher struct {
	sem chan struct{}
}

// NewHasher returns a hasher allowing n concurrent hashes; n <= 0 picks
// min(GOMAXPROCS, 4).
func NewHasher(n int) *Hasher {
	if n <= 0 {
		n = min(runtime.GOMAXPROCS(0), 4)
	}
	return &Hasher{sem: make(chan struct{}, n)}
}

func (h *Hasher) acquire(ctx context.Context) error {
	select {
	case h.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *Hasher) release() { <-h.sem }

// Hash returns the PHC string of password with a fresh random salt:
// $argon2id$v=19$m=19456,t=2,p=1$<salt>$<hash> (unpadded base64).
func (h *Hasher) Hash(ctx context.Context, password string) (string, error) {
	if len(password) > MaxPasswordLen {
		return "", ErrPasswordTooLong
	}
	salt := make([]byte, argonSaltLen)
	_, _ = rand.Read(salt)
	if err := h.acquire(ctx); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, ArgonTime, ArgonMemoryKiB, ArgonThreads, argonKeyLen)
	h.release()
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, ArgonMemoryKiB, ArgonTime, ArgonThreads,
		b64.EncodeToString(salt), b64.EncodeToString(key)), nil
}

var b64 = base64.RawStdEncoding

// Verify reports whether password matches the PHC string. The parameters
// recorded in the string are used, so hashes made with older parameters
// keep verifying. Passwords over MaxPasswordLen never match and are not
// hashed.
func (h *Hasher) Verify(ctx context.Context, phc, password string) (bool, error) {
	p, err := parsePHC(phc)
	if err != nil {
		return false, err
	}
	if len(password) > MaxPasswordLen {
		return false, nil
	}
	if err := h.acquire(ctx); err != nil {
		return false, err
	}
	key := argon2.IDKey([]byte(password), p.salt, p.t, p.m, p.p, uint32(len(p.key))) // #nosec G115 -- key length bounded by parsePHC
	h.release()
	return subtle.ConstantTimeCompare(key, p.key) == 1, nil
}

type phc struct {
	m, t      uint32
	p         uint8
	salt, key []byte
}

// parsePHC parses an Argon2id PHC string, bounding the parameters so a
// tampered hash cannot make verification allocate gigabytes.
func parsePHC(s string) (phc, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 6 || parts[0] != "" || parts[1] != "argon2id" || parts[2] != "v="+strconv.Itoa(argon2.Version) {
		return phc{}, ErrBadHash
	}
	var out phc
	for _, kv := range strings.Split(parts[3], ",") {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			return phc{}, ErrBadHash
		}
		n, err := strconv.ParseUint(v, 10, 32)
		if err != nil {
			return phc{}, ErrBadHash
		}
		switch k {
		case "m":
			out.m = uint32(n)
		case "t":
			out.t = uint32(n)
		case "p":
			if n > 255 {
				return phc{}, ErrBadHash
			}
			out.p = uint8(n)
		default:
			return phc{}, ErrBadHash
		}
	}
	if out.m < 8 || out.m > 256*1024 || out.t < 1 || out.t > 16 || out.p < 1 || out.p > 16 {
		return phc{}, ErrBadHash
	}
	var err error
	if out.salt, err = b64.DecodeString(parts[4]); err != nil || len(out.salt) < 8 {
		return phc{}, ErrBadHash
	}
	if out.key, err = b64.DecodeString(parts[5]); err != nil || len(out.key) < 16 || len(out.key) > 64 {
		return phc{}, ErrBadHash
	}
	return out, nil
}

// CheckNewPassword applies the rules for setting a password.
func CheckNewPassword(password string) error {
	switch {
	case len(password) > MaxPasswordLen:
		return ErrPasswordTooLong
	case len([]rune(password)) < MinPasswordLen:
		return ErrPasswordTooShort
	}
	return nil
}

// GeneratePassword returns a random 26-character password (crypto/rand
// Text: base32, 128 bits).
func GeneratePassword() string { return rand.Text() }
