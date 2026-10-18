package session

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func TestSealerRoundTrip(t *testing.T) {
	s, err := NewSealer(PurposeSession, testKey(1), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, pt := range [][]byte{{}, []byte("x"), bytes.Repeat([]byte("kudurru"), 300)} {
		v := s.Seal(pt)
		if len(v) != SealedLen(len(pt)) {
			t.Errorf("SealedLen(%d) = %d, actual %d", len(pt), SealedLen(len(pt)), len(v))
		}
		if strings.ContainsAny(v, "+/=") {
			t.Errorf("sealed value is not unpadded base64url: %q", v)
		}
		got, prev, err := s.Open(v)
		if err != nil || prev || !bytes.Equal(got, pt) {
			t.Fatalf("Open = %q, %v, %v", got, prev, err)
		}
	}
}

func TestSealerNonceIsRandom(t *testing.T) {
	s, _ := NewSealer(PurposeSession, testKey(1), nil)
	if a, b := s.Seal([]byte("same")), s.Seal([]byte("same")); a == b {
		t.Fatal("two seals of the same plaintext are identical; nonce is not random")
	}
}

func TestSealerRejectsTampering(t *testing.T) {
	s, _ := NewSealer(PurposeSession, testKey(1), nil)
	v := s.Seal([]byte(`{"v":1,"sid":"abc"}`))
	raw, _ := base64.RawURLEncoding.DecodeString(v)

	flip := func(i int) string {
		c := append([]byte{}, raw...)
		c[i] ^= 0x01
		return base64.RawURLEncoding.EncodeToString(c)
	}
	cases := map[string]string{
		"empty":            "",
		"not base64":       "!!!!",
		"padded base64":    v + "==",
		"truncated":        v[:len(v)-4],
		"too short":        base64.RawURLEncoding.EncodeToString(raw[:minSealed-1]),
		"version byte":     flip(0),
		"key id":           flip(1),
		"nonce":            flip(headerLen),
		"ciphertext":       flip(headerLen + nonceLen),
		"tag":              flip(len(raw) - 1),
		"appended garbage": base64.RawURLEncoding.EncodeToString(append(append([]byte{}, raw...), 0)),
	}
	for name, val := range cases {
		if _, _, err := s.Open(val); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: Open err = %v, want ErrInvalid", name, err)
		}
	}
}

func TestSealerPurposeSeparation(t *testing.T) {
	sess, _ := NewSealer(PurposeSession, testKey(1), nil)
	pre, _ := NewSealer(PurposeOIDCPreAuth, testKey(1), nil)
	if _, _, err := sess.Open(pre.Seal([]byte("x"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("session sealer opened a pre-auth value: %v", err)
	}
	if _, _, err := pre.Open(sess.Seal([]byte("x"))); !errors.Is(err, ErrInvalid) {
		t.Fatalf("pre-auth sealer opened a session value: %v", err)
	}
}

func TestSealerRotation(t *testing.T) {
	oldKey, newKey := testKey(1), testKey(2)
	before, _ := NewSealer(PurposeSession, oldKey, nil)
	oldValue := before.Seal([]byte("old"))

	during, err := NewSealer(PurposeSession, newKey, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	got, usedPrev, err := during.Open(oldValue)
	if err != nil || !usedPrev || string(got) != "old" {
		t.Fatalf("rotation window: Open(old) = %q, %v, %v", got, usedPrev, err)
	}
	newValue := during.Seal([]byte("new"))
	if _, usedPrev, err := during.Open(newValue); err != nil || usedPrev {
		t.Fatalf("rotation window: Open(new) usedPrevious=%v err=%v", usedPrev, err)
	}
	// A replica that has not been rolled yet (old key only) cannot read
	// new seals: roll every replica before relying on new cookies.
	if _, _, err := before.Open(newValue); !errors.Is(err, ErrInvalid) {
		t.Fatalf("old-only sealer opened a new-key value: %v", err)
	}

	after, _ := NewSealer(PurposeSession, newKey, nil)
	if _, _, err := after.Open(oldValue); !errors.Is(err, ErrInvalid) {
		t.Fatalf("after removing the previous key, old value still opens: %v", err)
	}
}

func TestNewSealerValidation(t *testing.T) {
	cases := map[string]struct {
		purpose  string
		cur, prv []byte
	}{
		"no purpose":         {"", testKey(1), nil},
		"short key":          {PurposeSession, testKey(1)[:16], nil},
		"long key":           {PurposeSession, append(testKey(1), 1), nil},
		"short previous":     {PurposeSession, testKey(1), []byte{1, 2, 3}},
		"previous = current": {PurposeSession, testKey(1), testKey(1)},
	}
	for name, c := range cases {
		if _, err := NewSealer(c.purpose, c.cur, c.prv); err == nil {
			t.Errorf("%s: NewSealer succeeded", name)
		}
	}
}

func TestParseKey(t *testing.T) {
	k := make([]byte, KeySize)
	for i := range k {
		k[i] = byte(i * 7)
	}
	for name, in := range map[string]string{
		"std":         base64.StdEncoding.EncodeToString(k),
		"raw std":     base64.RawStdEncoding.EncodeToString(k),
		"url":         base64.URLEncoding.EncodeToString(k),
		"raw url":     base64.RawURLEncoding.EncodeToString(k),
		"hex":         hex.EncodeToString(k),
		"file w/ nl":  base64.StdEncoding.EncodeToString(k) + "\n",
		"hex upper":   strings.ToUpper(hex.EncodeToString(k)),
		"leading spc": "  " + hex.EncodeToString(k),
	} {
		got, err := ParseKey(in)
		if err != nil || !bytes.Equal(got, k) {
			t.Errorf("%s: ParseKey = %x, %v", name, got, err)
		}
	}
	for _, bad := range []string{"", "short", base64.StdEncoding.EncodeToString(k[:16]), strings.Repeat("z", 64)} {
		if _, err := ParseKey(bad); err == nil {
			t.Errorf("ParseKey(%q) succeeded", bad)
		}
	}
}
