package main

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
)

func TestServingHTTPPasswordIntermediateClockRegression(t *testing.T) {
	f := newServingHTTPFixture(t, true)
	defer f.stop() // Join the writer before testing cancels t.Context.
	cookie, info := f.login(t)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	var original auth.Session
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		if !v.Base().Same(before.Generation()) {
			return false
		}
		var err error
		original, err = v.CookieSession(cookie.Value, time.Now())
		return err == nil && f.s.publisher.Status().ProtectedReady
	})

	authenticatedAt := time.Now().UTC()
	regressedAt := original.IssuedAt.Add(-time.Second)
	recoveredAt := authenticatedAt.Add(time.Nanosecond)
	var phase atomic.Uint32
	f.s.opts.Now = func() time.Time {
		// Isolate this actual password request's clock from background work and
		// from unrelated preflight reads. Observe the real raw-proof helper,
		// without relying on how many clocks preparation or admission consults.
		var pcs [64]uintptr
		frames := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs[:])])
		var passwordRequest, authenticating bool
		for {
			frame, more := frames.Next()
			passwordRequest = passwordRequest || strings.Contains(frame.Function, ".(*servingHTTP).password")
			authenticating = authenticating || strings.HasSuffix(frame.Function, "/servingauth.AuthenticateCurrentWithClock")
			if !more {
				break
			}
		}
		if !passwordRequest {
			return time.Now()
		}
		if authenticating && phase.CompareAndSwap(0, 1) {
			return authenticatedAt
		}
		if phase.CompareAndSwap(1, 2) {
			// Before the fix this is the independent mutation-floor sample.
			// If the floor is retained from authentication instead, a regressed
			// issuance sample may safely abort the entire command.
			return regressedAt
		}
		if phase.Load() == 2 {
			return recoveredAt
		}
		return authenticatedAt
	}

	changed := f.call(t, http.MethodPost, "/auth/password",
		`{"currentPassword":"initial-password","newPassword":"replacement-password"}`,
		"", cookie, info.CSRF)
	if phase.Load() != 2 {
		t.Fatalf("request did not reach current raw authentication and the intermediate clock regression: phase=%d HTTP=%d", phase.Load(), changed.Code)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		return v.Base().Same(after.Generation()) && f.s.publisher.Status().ProtectedReady
	})
	old := f.call(t, http.MethodGet, "/auth/me", "", "", cookie, "")
	switch changed.Code {
	case http.StatusUnauthorized:
		if !before.Generation().Same(after.Generation()) {
			t.Fatal("clock refusal partially committed the password, status, floor, or enrollment")
		}
		expectServingHTTPStatus(t, old, http.StatusOK)
		t.Log("intermediate clock regression refused with complete durable rollback; original cookie remains valid")
	case http.StatusOK:
		if before.Generation().Same(after.Generation()) {
			t.Fatal("password success did not commit a new generation")
		}
		state, err := servingauth.DecodeSecurity(after.Payload.Security)
		if err != nil {
			t.Fatal(err)
		}
		var floor time.Time
		for _, candidate := range state.AccountFloors {
			if candidate.Account.Kind == "LocalUser" && candidate.Account.Name == original.Principal.Name && candidate.Account.UID == original.Principal.AccountUID {
				floor = candidate.ValidAfter
			}
		}
		if old.Code != http.StatusUnauthorized {
			t.Fatalf("password change committed but old cookie remains usable: HTTP %d, issued=%s floor=%s verified=%s recovered=%s",
				old.Code, original.IssuedAt.Format(time.RFC3339Nano), floor.Format(time.RFC3339Nano), authenticatedAt.Format(time.RFC3339Nano), recoveredAt.Format(time.RFC3339Nano))
		}
		var out api.SessionInfo
		if json.Unmarshal(changed.Body.Bytes(), &out) != nil || out.MustChangePassword || out.CSRF == "" || len(changed.Result().Cookies()) != 1 {
			t.Fatal("successful password change did not replace the restricted enrollment")
		}
		expectServingHTTPStatus(t, f.call(t, http.MethodGet, "/auth/me", "", "", changed.Result().Cookies()[0], ""), http.StatusOK)
		t.Log("password change committed with old-cookie denial and a valid replacement cookie")
	default:
		t.Fatalf("password regression returned unexpected HTTP %d", changed.Code)
	}
}
