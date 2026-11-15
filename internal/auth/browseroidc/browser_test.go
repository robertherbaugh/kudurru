package browseroidc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

var browserNow = time.Date(2026, 10, 15, 12, 30, 0, 123, time.UTC)

func TestCodecRotationPurposeSeparationAndCanonicalPendingBinding(t *testing.T) {
	first, err := NewCodec(bytes.Repeat([]byte{1}, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	state, err := New("corp", "/zones?tab=records", browserNow)
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := first.Seal(state, browserNow)
	if err != nil {
		t.Fatal(err)
	}
	rotated, err := NewCodec(bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := rotated.Open(cookie, browserNow.Add(time.Second))
	if err != nil || opened != state || opened.PayloadHash() != state.PayloadHash() {
		t.Fatal("rotation/binding", err)
	}
	if len(state.PayloadHash()) != 64 || !state.MatchState(state.OAuthState()) || state.MatchState(strings.Repeat("x", 43)) {
		t.Fatal("state/hash")
	}
	if state.ID() == state.Nonce() || state.Nonce() == state.OAuthState() || state.OAuthState() == state.Verifier() {
		t.Fatal("random domains reused")
	}
	if state.Provider() != "corp" || state.Next() != "/zones?tab=records" || !state.ExpiresAt().Equal(state.IssuedAt().Add(Lifetime)) {
		t.Fatal("projection")
	}
	for _, purpose := range []string{session.PurposeOIDCPreAuth, session.PurposeSession} {
		legacy, _ := session.NewSealer(purpose, bytes.Repeat([]byte{1}, 32), nil)
		raw, _ := json.Marshal(state.value)
		if _, err := first.Open(legacy.Seal(raw), browserNow); err == nil {
			t.Fatal("foreign purpose accepted")
		}
		if _, _, err := legacy.Open(cookie); err == nil {
			t.Fatal("preauth accepted as session")
		}
	}
	for _, now := range []time.Time{browserNow.Add(-time.Nanosecond), state.ExpiresAt(), state.ExpiresAt().Add(time.Second), {}} {
		if _, err := first.Open(cookie, now); !errors.Is(err, ErrTime) {
			t.Fatal("time accepted", err)
		}
	}
	if _, err := first.Open(strings.Repeat("A", MaxCookieBytes+1), browserNow); !errors.Is(err, ErrBounds) {
		t.Fatal("cookie cap", err)
	}
	if _, err := first.Open(cookie[:len(cookie)-5], browserNow); err == nil {
		t.Fatal("truncation accepted")
	}
	if _, err := first.Seal(State{}, browserNow); err == nil {
		t.Fatal("zero state")
	}
	if _, err := NewCodec([]byte("short"), nil); err == nil {
		t.Fatal("key cap")
	}
	if _, err := New("BAD_PROVIDER", "", browserNow); err == nil {
		t.Fatal("provider")
	}
	if _, err := New("corp", "", time.Time{}); err == nil {
		t.Fatal("zero clock")
	}
}

func TestAuthenticatedMalformedPayloadsAndRedaction(t *testing.T) {
	c, _ := NewCodec(bytes.Repeat([]byte{3}, 32), nil)
	s, _ := New("corp", "/", browserNow)
	raw, _ := json.Marshal(s.value)
	for name, data := range map[string][]byte{
		"duplicate":    bytes.Replace(raw, []byte(`"formatVersion":1`), []byte(`"formatVersion":1,"formatVersion":1`), 1),
		"unknown":      append(append([]byte{}, raw[:len(raw)-1]...), []byte(`,"unknown":true}`)...),
		"version":      bytes.Replace(raw, []byte(`"formatVersion":1`), []byte(`"formatVersion":2`), 1),
		"missing":      bytes.Replace(raw, []byte(`"formatVersion":1,`), nil, 1),
		"trailing":     append(append([]byte{}, raw...), []byte(` {}`)...),
		"noncanonical": append([]byte(" "), raw...),
		"oversize":     bytes.Repeat([]byte(" "), maxPlainBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := c.Open(c.sealer.Seal(data), browserNow); err == nil {
				t.Fatal("malformed payload accepted")
			}
		})
	}
	for _, value := range []any{s, c} {
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{fmt.Sprint(value), fmt.Sprintf("%#v", value), string(encoded)} {
			for _, secret := range []string{s.ID(), s.OAuthState(), s.Nonce(), s.Verifier()} {
				if strings.Contains(text, secret) {
					t.Fatal("private state disclosed")
				}
			}
		}
	}
}

func TestRFC7636AndSafeNext(t *testing.T) {
	challenge, err := S256("dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk")
	if err != nil || challenge != "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM" {
		t.Fatal("RFC vector", err)
	}
	for _, v := range []string{"", strings.Repeat("a", 42), strings.Repeat("a", 129), strings.Repeat("a", 42) + "+", strings.Repeat("a", 42) + "é"} {
		if _, err := S256(v); err == nil {
			t.Fatal("bad verifier")
		}
	}
	for _, raw := range []string{"", "/", "/zones", "/zones/", "/zones?tab=all", "/search?q=https%3A%2F%2Fexample.com"} {
		if _, err := SafeNext(raw); err != nil {
			t.Fatal("safe next denied", err)
		}
	}
	for _, raw := range []string{"https://evil.example", "//evil.example", "/\\evil", "/%2fevil", "/%5Cevil", "/%252fevil", "/a/../evil", "/a/%2e%2e/evil", "/a//b", "/a#fragment", "/a#", "/?q=%0a", "/%00", "/%", "/\x7f", string([]byte{'/', 0xff}), strings.Repeat("/", MaxNextBytes+1)} {
		if _, err := SafeNext(raw); err == nil {
			t.Fatal("unsafe next accepted")
		}
	}
}

func FuzzCookieAndNextBounds(f *testing.F) {
	f.Add("/", "bad-cookie")
	f.Add("//evil.example", strings.Repeat("A", MaxCookieBytes+1))
	c, _ := NewCodec(bytes.Repeat([]byte{9}, 32), nil)
	f.Fuzz(func(t *testing.T, next, cookie string) {
		if got, err := SafeNext(next); err == nil {
			if again, err := SafeNext(got); err != nil || again != got {
				t.Fatal("non-idempotent next")
			}
		}
		if state, err := c.Open(cookie, browserNow); err == nil && state.ValidAt(browserNow) != nil {
			t.Fatal("invalid decoded state")
		}
	})
}
