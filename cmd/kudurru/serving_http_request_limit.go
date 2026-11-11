package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/auth/requestlimit"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// providerLoginFollowerHint is the next step for OIDC browser and CLI login
// on an enrolled follower, which admits local password login only.
const providerLoginFollowerHint = "OIDC login runs on the primary: open the primary's address to sign in with a provider, or sign in on this node with a local username and password"

type requestAdmissionKey struct{}
type requestAdmission struct {
	owner    *servingHTTP
	lane     requestlimit.Lane
	original context.Context
	used     atomic.Bool
}

func (h *servingHTTP) requestLane(r *http.Request) requestlimit.Lane {
	path, ok := strings.CutPrefix(r.URL.Path, h.opts.RoutePrefix+"/auth/")
	if !ok {
		return ""
	}
	parts := strings.Split(path, "/")
	if len(parts) != 3 || parts[1] == "" {
		return ""
	}
	if parts[0] == "oidc" && r.Method == http.MethodGet {
		switch parts[2] {
		case "start":
			return requestlimit.Start
		case "callback":
			return requestlimit.Callback
		}
	}
	if parts[0] == "cli" && parts[2] == "prepare" && r.Method == http.MethodPost {
		return requestlimit.CLI
	}
	return ""
}

// requestAdmissionMiddleware executes BEFORE the adapter's first complete
// lease capture and any credential interpretation. This command charges only
// operational counters; it cannot authorize provider I/O or authentication.
// Actual success permits the existing adapter to start its original capture.
func (h *servingHTTP) requestAdmissionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lane := h.requestLane(r)
		if lane == "" {
			next.ServeHTTP(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if lane != requestlimit.Callback {
			if err := h.opts.CrossOrigin.Check(r); err != nil {
				servingHTTPError(w, r, &auth.Error{Status: 403, Key: "csrf", Message: "cross-origin login rejected"})
				return
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), requestlimit.Lifetime)
		defer cancel()
		s := h.opts.Runtime
		if s.primary == nil || s.primary.driver == nil || s.opts.Store == nil {
			servingHTTPError(w, r, servingBrowserUnavailable())
			return
		}
		status := s.primary.driver.Status()
		if !status.Running {
			// The charge below needs the writer. An enrolled follower has none:
			// provider login runs on its primary, and the answer says so.
			if refusal, follower := s.storePrimaryRequired(providerLoginFollowerHint); follower {
				servingHTTPError(w, r, refusal)
				return
			}
			servingHTTPError(w, r, servingBrowserUnavailable())
			return
		}
		ctx, release, err := s.primary.writer.bind(ctx, status.Writer)
		if err != nil {
			servingHTTPError(w, r, servingBrowserUnavailable())
			return
		}
		defer release()
		check := func(ctx context.Context) error {
			if !carriedWriterCurrent(ctx) {
				return replication.ErrFenced
			}
			return nil
		}
		err = s.opts.Store.Command(ctx, status.Writer.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := check(ctx); err != nil {
				return err
			}
			if err := tx.RequireCommitCheck(check); err != nil {
				return err
			}
			return requestlimit.Charge(ctx, tx, lane, h.clientKey(r), h.now)
		})
		if err != nil {
			var locked *loginlimit.Locked
			if errors.As(err, &locked) {
				servingHTTPError(w, r, &auth.Error{Status: 429, Key: "rate_limited", Message: "too many login requests; try again later", RetryAfter: locked.RetryAfter})
			} else {
				servingHTTPError(w, r, servingBrowserUnavailable())
			}
			return
		}
		// The charge remains durable if cancellation arrives after commit.
		// Do not refund, retry or capture anything on that failed request.
		s.publisher.Wake()
		if check(ctx) != nil {
			servingHTTPError(w, r, servingBrowserUnavailable())
			return
		}
		admission := &requestAdmission{owner: h, lane: lane, original: ctx}
		next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, requestAdmissionKey{}, admission)))
	})
}

func (h *servingHTTP) checkRequestAdmission(r *http.Request, lane requestlimit.Lane) error {
	admission, _ := r.Context().Value(requestAdmissionKey{}).(*requestAdmission)
	if admission == nil || admission.owner != h || admission.lane != lane || admission.original.Err() != nil || !carriedWriterCurrent(r.Context()) || !admission.used.CompareAndSwap(false, true) {
		return servingBrowserUnavailable()
	}
	return nil
}
