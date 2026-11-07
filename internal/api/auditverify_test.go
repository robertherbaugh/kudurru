package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

type verificationReader struct {
	audit.Memory
	verify func(context.Context) error
}

func (r *verificationReader) Verify(ctx context.Context) error { return r.verify(ctx) }

func TestAuditVerificationClassifiesAndCachesFailures(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"success", nil, http.StatusOK},
		{"integrity", fmt.Errorf("%w: private stored details", audit.ErrTampered), http.StatusOK},
		{"storage", errors.New("private driver details"), http.StatusServiceUnavailable},
		{"timeout", fmt.Errorf("private timeout details: %w", context.DeadlineExceeded), http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			now := time.Now()
			rd := &verificationReader{verify: func(context.Context) error { calls++; return tc.err }}
			h := newHarness(t, func(o *Options) {
				o.AuditReader = rd
				o.Now = func() time.Time { return now }
			})
			for range 3 {
				res := h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(tc.code)
				if strings.Contains(res.Body.String(), "private") {
					t.Fatal("verification exposed backend or stored details")
				}
				if tc.code == http.StatusOK {
					var result AuditVerification
					res.json(&result)
					if result.Verified != (tc.err == nil) || !result.CheckedAt.Equal(now) || (tc.err != nil && result.Reason == "") {
						t.Fatalf("verification: %+v", result)
					}
				}
			}
			if calls != 1 {
				t.Fatalf("%d scans, want one cached result", calls)
			}
			h.do(http.MethodGet, "/api/v1/audit/verify", "viewer", nil).want(http.StatusForbidden)
			now = now.Add(auditVerificationTTL)
			h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(tc.code)
			if calls != 2 {
				t.Fatalf("%d scans after cache expiry, want two", calls)
			}
		})
	}
}

func TestAuditVerificationSharesScanAndSurvivesCanceledCaller(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	rd := &verificationReader{verify: func(ctx context.Context) error {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-release:
			return nil
		}
	}}
	h := newHarness(t, func(o *Options) { o.AuditReader = rd })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	canceled := make(chan error, 1)
	go func() {
		_, err := h.srv.cachedAuditVerification(ctx, rd)
		canceled <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("scan did not start")
	}
	cancel()
	select {
	case err := <-canceled:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled caller: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled caller waited for the shared scan")
	}
	const callers = 20
	results := make(chan auditVerificationResult, callers)
	for range callers {
		go func() {
			value, err := h.srv.cachedAuditVerification(context.Background(), rd)
			if err != nil {
				value.err = err
			}
			results <- value
		}()
	}
	close(release)
	for range callers {
		select {
		case value := <-results:
			if value.err != nil || !value.result.Verified {
				t.Fatalf("shared scan: %+v", value)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("shared scan did not finish")
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d scans, want one", calls.Load())
	}
}

type snapshotVerificationReader struct {
	verificationReader
	snapshot func(context.Context) (audit.VerificationSnapshot, error)
}

func (r *snapshotVerificationReader) SnapshotVerification(ctx context.Context) (audit.VerificationSnapshot, error) {
	return r.snapshot(ctx)
}

func TestAuditVerificationTrustCaptureAndCacheSupersession(t *testing.T) {
	var revision atomic.Int32
	revision.Store(1)
	var scans, captures atomic.Int32
	rd := &snapshotVerificationReader{}
	rd.snapshot = func(ctx context.Context) (audit.VerificationSnapshot, error) {
		captures.Add(1)
		if err := ctx.Err(); err != nil {
			return audit.VerificationSnapshot{}, err
		}
		original := revision.Load()
		if original == 0 {
			return audit.VerificationSnapshot{}, errors.New("private trust transport failure")
		}
		return audit.VerificationSnapshot{Identity: fmt.Sprint(original), Verifier: audit.VerifierFunc(func(context.Context) error { scans.Add(1); return nil }), Current: func(context.Context) error {
			if revision.Load() != original {
				return errors.New("superseded trust")
			}
			return nil
		}}, nil
	}
	h := newHarness(t, func(o *Options) { o.AuditReader = rd })
	for range 2 {
		h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(http.StatusOK)
	}
	if scans.Load() != 1 || captures.Load() != 2 {
		t.Fatal("cache did not capture current trust before reuse")
	}
	h.do(http.MethodGet, "/api/v1/audit/verify", "viewer", nil).want(http.StatusForbidden)
	if captures.Load() != 2 {
		t.Fatal("unauthorized caller read trust")
	}
	revision.Store(2)
	h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(http.StatusOK)
	if scans.Load() != 2 {
		t.Fatal("successor shared stale singleflight/cache")
	}
	revision.Store(0)
	response := h.do(http.MethodGet, "/api/v1/audit/verify", "auditor", nil).want(http.StatusServiceUnavailable)
	if strings.Contains(response.Body.String(), "private") {
		t.Fatal("exposed trust transport error")
	}
	if scans.Load() != 2 {
		t.Fatal("scanned with unavailable trust")
	}
}

func TestAuditVerificationHeldOriginalTrustRefusesReplacement(t *testing.T) {
	var revision atomic.Int32
	revision.Store(1)
	started, release := make(chan struct{}), make(chan struct{})
	var scans atomic.Int32
	rd := &snapshotVerificationReader{}
	rd.snapshot = func(context.Context) (audit.VerificationSnapshot, error) {
		original := revision.Load()
		return audit.VerificationSnapshot{Identity: fmt.Sprint(original), Verifier: audit.VerifierFunc(func(ctx context.Context) error {
			scans.Add(1)
			close(started)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-release:
				return nil
			}
		}), Current: func(context.Context) error {
			if revision.Load() != original {
				return errors.New("superseded")
			}
			return nil
		}}, nil
	}
	h := newHarness(t, func(o *Options) { o.AuditReader = rd })
	done := make(chan error, 1)
	go func() { _, err := h.srv.cachedAuditVerification(t.Context(), rd); done <- err }()
	<-started
	revision.Store(2)
	close(release)
	if err := <-done; err == nil || scans.Load() != 1 {
		t.Fatal("replayed original scan under successor")
	}
	if _, ok := h.srv.auditVerification.cached(time.Now(), "1"); ok {
		t.Fatal("published superseded trust result")
	}
}
