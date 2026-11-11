package api

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/config"
)

// forwardSettingsStub is a canonical settings source whose writer must never
// run: on a forwarding node every cluster patch reaches SettingsForward.
type forwardSettingsStub struct {
	cfg     *config.Config
	written int
}

func (f *forwardSettingsStub) Settings(context.Context) (*config.Config, error) { return f.cfg, nil }
func (f *forwardSettingsStub) WriteSettings(context.Context, *config.Config, []string) error {
	f.written++
	return nil
}

// The PATCH /settings seam: a canonical cluster patch is handed to the
// forward with its merge patch and If-Match, after the route's own input
// checks and before any local writer; the answer names this node. A handled
// error is the route's error; handled=false runs the local command.
func TestSettingsForwardSeam(t *testing.T) {
	var got []ForwardedSettings
	handled := true
	var answer error
	stub := &forwardSettingsStub{}
	h := newHarness(t, func(o *Options) {
		stub.cfg = o.Config
		o.CanonicalSettings = stub
		o.SettingsForward = func(_ context.Context, in ForwardedSettings) (*SettingsBody, bool, error) {
			got = append(got, in)
			if !handled {
				return nil, false, nil
			}
			if answer != nil {
				return nil, true, answer
			}
			persisted := true
			return &SettingsBody{Scope: "cluster", Node: "the-primary", Settings: map[string]any{}, Changed: []string{"blocking.blockTTL"}, Persisted: &persisted}, true, nil
		}
	})
	patch := map[string]any{"blocking": map[string]any{"blockTTL": "42s"}}
	var body SettingsBody
	h.do(http.MethodPatch, BasePath+"/settings", "admin", patch, "If-Match", `"3"`).want(http.StatusOK).json(&body)
	if len(got) != 1 || got[0].IfMatch != `"3"` || got[0].Patch["blocking"].(map[string]any)["blockTTL"] != "42s" || stub.written != 0 {
		t.Fatalf("forward input: %+v written=%d", got, stub.written)
	}
	if body.Node != "node-a" || body.Changed[0] != "blocking.blockTTL" {
		t.Fatalf("forwarded answer does not name the answering node: %+v", body)
	}
	// The route's own input checks run first and send nothing.
	h.do(http.MethodPatch, BasePath+"/settings", "admin", map[string]any{}).want(http.StatusBadRequest)
	h.do(http.MethodPatch, BasePath+"/settings", "admin", map[string]any{"noSuchKey": 1}).want(http.StatusBadRequest)
	if len(got) != 1 {
		t.Fatal("an invalid patch reached the forward")
	}
	// A handled refusal is the route's answer; nothing falls back.
	answer = &Error{Status: http.StatusConflict, Body: ErrorBody{Key: KeyConflict, Message: "changed"}}
	if r := h.do(http.MethodPatch, BasePath+"/settings", "admin", patch); r.Code != http.StatusConflict || r.errKey() != KeyConflict || stub.written != 0 {
		t.Fatalf("handled refusal: %d %s", r.Code, r.Body)
	}
	answer = errors.New("exchange failed")
	if r := h.do(http.MethodPatch, BasePath+"/settings", "admin", patch); r.Code < 500 || stub.written != 0 {
		t.Fatalf("handled failure fell back or succeeded: %d %s", r.Code, r.Body)
	}
	// handled=false: the node runs its own canonical command.
	handled, answer = false, nil
	h.do(http.MethodPatch, BasePath+"/settings", "admin", patch).want(http.StatusOK)
	if stub.written != 1 {
		t.Fatal("an unhandled forward did not run the local command")
	}
}

func TestForwardedSettingsRefusalClassification(t *testing.T) {
	if _, ok := ForwardedSettingsRefusal(errInvalid("bad")); !ok {
		t.Fatal("invalid input is not a retained refusal")
	}
	if _, ok := ForwardedSettingsRefusal(&forwardedSettingsWriteError{errConflict("write", "")}); ok {
		t.Fatal("a failed staged write was classified as a retained refusal")
	}
	if _, ok := ForwardedSettingsRefusal(newError(http.StatusForbidden, KeyForbidden, "denied", "")); ok {
		t.Fatal("an authorization failure was classified as a retained refusal")
	}
}
