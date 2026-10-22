package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestZoneNotifyCommandBypassesLegacyAndDetachesResponse(t *testing.T) {
	legacy := 0
	result := &NotifyResult{Zone: "example", Targets: []string{"192.0.2.53:53"}}
	calls := 0
	h := newHarness(t, func(o *Options) {
		o.Notifier = notifyFunc(func(string) ([]string, error) { legacy++; return nil, nil })
		o.ZoneNotifyCommand = func(_ context.Context, name string) (*NotifyResult, error) {
			calls++
			if name != "example" {
				t.Fatal("wrong metadata.name", name)
			}
			return result, nil
		}
	})
	// No live desired object exists: only the current capability may select it.
	var body NotifyResult
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusAccepted).json(&body)
	if calls != 1 || legacy != 0 || len(body.Targets) != 1 {
		t.Fatal(calls, legacy, body)
	}
	out, err := h.srv.notifyZoneCommand(t.Context(), &zonePathInput{Zone: "example"})
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Targets[0] = "mutated"
	if result.Targets[0] != "192.0.2.53:53" {
		t.Fatal("result aliases capability")
	}
	for _, path := range []string{"/api/v1/zones/example/notify", "/api/v1/zones/other/notify"} {
		h.do(http.MethodPost, path, "operator", nil).want(http.StatusForbidden)
	}
	if calls != 2 {
		t.Fatal("outer denied authority reached callback")
	}
}

func TestZoneNotifyCommandCanonicalAbsenceAndResultBounds(t *testing.T) {
	h := newHarness(t, func(o *Options) {
		o.ObjectCommand = func(context.Context, store.Key, func(context.Context, *sqlite.CommandTx, ObjectAuthorize) error) error {
			return nil
		}
		o.Notifier = notifyFunc(func(string) ([]string, error) { t.Fatal("legacy fallback"); return nil, nil })
	})
	h.seed("api", zoneObj("example", "example.com."))
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusServiceUnavailable)
	for _, result := range []*NotifyResult{nil, {Zone: "wrong"}, {Zone: "example", Targets: make([]string, 65)}, {Zone: "example", Targets: []string{strings.Repeat("x", 513)}}} {
		h.srv.opts.ZoneNotifyCommand = func(context.Context, string) (*NotifyResult, error) { return result, nil }
		h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusServiceUnavailable)
	}
	h.srv.opts.ZoneNotifyCommand = func(context.Context, string) (*NotifyResult, error) { return &NotifyResult{Zone: "example"}, nil }
	res := h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusAccepted)
	if !strings.Contains(res.Body.String(), `"targets":[]`) {
		t.Fatal(res.Body)
	}
	h.srv.opts.ZoneNotifyCommand = func(context.Context, string) (*NotifyResult, error) { return nil, errConflict("pending source", "") }
	h.do(http.MethodPost, "/api/v1/zones/example/notify", "zoneadmin", nil).want(http.StatusConflict)
}
