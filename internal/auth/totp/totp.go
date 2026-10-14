// Package totp implements bounded RFC6238 proof checks and pure durable replay
// transitions. It performs no I/O, authorization, enrollment or replay writes.
package totp

import (
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // G505: HMAC-SHA1 is the approved RFC6238 compatibility profile.
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"time"
)

// Errors are safe categories and never contain a seed, code or private payload.
var (
	ErrInvalid = errors.New("totp: invalid owner input")
	ErrVersion = errors.New("totp: unsupported owner version")
	ErrBounds  = errors.New("totp: owner bounds exceeded")
	ErrProof   = errors.New("totp: proof rejected")
	ErrReplay  = errors.New("totp: consumed step")
)

// Fixed production profile and codec bounds.
const (
	Period         = 30
	Digits         = 6
	Window         = 1
	FormatVersion  = 1
	MaxRecordBytes = 4096
	MaxSealedBytes = 2048
	MaxSeedBytes   = 1024
	Purpose        = "totp-seed/v1"
)

// Match verifies all three bounded candidates and returns the largest matching
// step. The caller must check/advance current replay state before granting login.
func Match(seed *Seed, code string, now time.Time) (uint64, error) {
	if seed == nil || !validAccount(seed.account) || seed.version == 0 || !validID(seed.id) || !validCode(code, Digits) || now.Unix() < 0 || now.UTC().Year() > 9999 {
		return 0, ErrProof
	}
	step := uint64(now.Unix() / Period) // #nosec G115 -- year bound makes Unix nonnegative.
	var matched uint64
	found := false
	for offset := -Window; offset <= Window; offset++ {
		if offset < 0 && step == 0 {
			continue
		}
		candidate := step
		if offset < 0 {
			candidate--
		} else if offset > 0 {
			candidate++
		}
		want, _ := rfcCode(seed.key[:], candidate, Digits, sha1.New)
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			matched, found = candidate, true
		}
	}
	if !found {
		return 0, ErrProof
	}
	return matched, nil
}

func validCode(code string, digits int) bool {
	if len(code) != digits {
		return false
	}
	for i := range code {
		if code[i] < '0' || code[i] > '9' {
			return false
		}
	}
	return true
}

// rfcCode is deliberately internal: production always uses the fixed profile.
func rfcCode(key []byte, step uint64, digits int, algorithm func() hash.Hash) (string, error) {
	if len(key) < 20 || len(key) > 64 || (digits != 6 && digits != 8) || algorithm == nil {
		return "", ErrInvalid
	}
	var counter [8]byte
	binary.BigEndian.PutUint64(counter[:], step)
	mac := hmac.New(algorithm, key)
	_, _ = mac.Write(counter[:])
	sum := mac.Sum(nil)
	if len(sum) != sha1.Size && len(sum) != sha256.Size && len(sum) != sha512.Size {
		return "", ErrInvalid
	}
	offset := int(sum[len(sum)-1] & 15)
	value := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	modulo := uint32(1000000)
	if digits == 8 {
		modulo = 100000000
	}
	return fmt.Sprintf("%0*d", digits, value%modulo), nil
}
