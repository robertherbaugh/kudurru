package cache

import (
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

func TestSetDisabledBypassesAndSetSettingsClamps(t *testing.T) {
	h := newHarness(t, nil)
	h.serve(request("a.example", dns.TypeA, 1))
	if r := h.serve(request("a.example", dns.TypeA, 2)); r.Status != pipeline.StatusCached {
		t.Fatalf("status %v", r.Status)
	}
	h.stage.SetDisabled(true)
	if r := h.serve(request("a.example", dns.TypeA, 3)); r.Status != pipeline.StatusForwarded || h.up.calls.Load() != 2 {
		t.Fatalf("disabled: %v calls %d", r.Status, h.up.calls.Load())
	}
	h.stage.SetDisabled(false)

	set := *h.stage.settings()
	set.MaxTTL = 60 * time.Second
	h.stage.SetSettings(set)
	h.up.set(forwarded("192.0.2.9", 3000))
	h.serve(request("b.example", dns.TypeA, 4))
	r := h.serve(request("b.example", dns.TypeA, 5))
	if r.Status != pipeline.StatusCached {
		t.Fatalf("status %v", r.Status)
	}
	for _, ttl := range ttls(r.Msg) {
		if ttl > 60 {
			t.Fatalf("maxTTL after SetSettings not applied: %v", ttls(r.Msg))
		}
	}
}
