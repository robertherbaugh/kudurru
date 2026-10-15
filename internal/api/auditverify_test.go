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
