package clioidc

import (
	"strings"
	"testing"
	"time"
)

func TestCLIReceiptCorrelationIsReusableAndPurposeSeparated(t *testing.T) {
	now := time.Now()
	source := Source{"corp", "original-uid", "config", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), "https://issuer.test", "cli"}
	q := PrepareRequest{"browser", strings.Repeat("A", 43), "http://127.0.0.1:8000/callback"}
	r, e := NewReceipt(source, q, now)
	if e != nil {
		t.Fatal("receipt construction")
	}
	c, e := NewCodec([]byte(strings.Repeat("k", 32)), nil)
	if e != nil {
		t.Fatal("codec")
	}
	raw, e := c.Seal(r, now)
	if e != nil {
		t.Fatal("seal")
	}
	for range 2 {
		got, e := c.Open(raw, now.Add(time.Second))
		if e != nil || got.Source() != source || got.Request() != q {
			t.Fatal("reusable source correlation")
		}
	}
	for _, at := range []time.Time{now.Add(-time.Nanosecond), now.Add(Lifetime), now.Add(Lifetime + time.Second)} {
		if _, e := c.Open(raw, at); e == nil {
			t.Fatal("original time boundary")
		}
	}
	other, _ := NewCodec([]byte(strings.Repeat("x", 32)), nil)
	if _, e := other.Open(raw, now); e == nil {
		t.Fatal("foreign cluster")
	}
	rotated, _ := NewCodec([]byte(strings.Repeat("x", 32)), []byte(strings.Repeat("k", 32)))
	if _, e := rotated.Open(raw, now); e != nil {
		t.Fatal("original previous-key source")
	}
	for _, bad := range []PrepareRequest{{"browser", q.Nonce, "http://localhost:8000/callback"}, {"browser", q.Nonce, "http://127.0.0.1:8001/callback"}, {"browser", "wrong", q.RedirectURI}, {"device", q.Nonce, q.RedirectURI}} {
		if bad.Validate() == nil {
			t.Fatal("invalid public request")
		}
	}
}

func TestCLIDeviceReceiptHasNoInventedBrowserNonce(t *testing.T) {
	now := time.Now()
	source := Source{"corp", "uid", "config", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), "https://issuer.test", "cli"}
	r, e := NewReceipt(source, PrepareRequest{Mode: "device"}, now)
	if e != nil {
		t.Fatal("device correlation")
	}
	codec, _ := NewCodec([]byte(strings.Repeat("k", 32)), nil)
	raw, e := codec.Seal(r, now)
	if e != nil {
		t.Fatal("seal device")
	}
	for range 2 {
		got, e := codec.Open(raw, now)
		if e != nil || got.Request().Mode != "device" || got.Request().Nonce != "" {
			t.Fatal("reusable non-authoritative device receipt")
		}
	}
	for _, q := range []PrepareRequest{{Mode: "device", Nonce: strings.Repeat("A", 43)}, {Mode: "device", RedirectURI: "http://127.0.0.1:8000/callback"}} {
		if q.Validate() == nil {
			t.Fatal("mixed device/browser correlation")
		}
	}
}
