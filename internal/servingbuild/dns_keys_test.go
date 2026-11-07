package servingbuild

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
)

func TestDNSKeysRetainOriginalCompletePublication(t *testing.T) {
	first, second := ring(t, "a"), ring(t, "b")
	keys, err := NewKeyringOwner(first)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keys)
	capture := func() *pipeline.Request {
		t.Helper()
		req := &pipeline.Request{Ctx: t.Context()}
		req.Serving, err = servingdns.Capture(f.pub)(req)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(req.Serving.Release)
		return req
	}
	original := capture()
	if got, err := DNSKeys(original); err != nil || got != first {
		t.Fatal("DNS selector did not use original complete runtime owner")
	}
	if err := keys.Replace(t.Context(), second); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	fresh := capture()
	oldKeys, oldErr := DNSKeys(original)
	newKeys, newErr := DNSKeys(fresh)
	if oldErr != nil || newErr != nil || oldKeys != first || newKeys != second {
		t.Fatal("DNS selector adopted a successor or repaired an old publication")
	}
	f.epoch.Add(1)
	if got, err := DNSKeys(original); err != nil || got != first {
		t.Fatal("held read lost original keys when another dependency changed")
	}
	if _, err := DNSKeys(&pipeline.Request{Ctx: t.Context()}); err == nil {
		t.Fatal("missing publication substituted current keys")
	}
}
