package cloudflare

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/ledger"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type idleTransport struct{ closed atomic.Bool }

func (*idleTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New("not called")
}
func (t *idleTransport) CloseIdleConnections() { t.closed.Store(true) }
func TestTransportOriginContainmentBodyBoundContextAndCleanup(t *testing.T) {
	f := newFixture(t)
	calls := 0
	rt := &requestTransport{host: f.h, origin: "https://api.example/client/v4/", token: testToken, base: transportFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !r.Close || r.Context().Value(contextKey{}) != "original" {
			t.Error("transport replaced scope or enabled automatic retry")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"success":true,"result":{}}`)), Header: make(http.Header)}, nil
	})}
	for _, url := range []string{"https://other.example/client/v4/zones", "http://api.example/client/v4/zones", "https://user:password@api.example/client/v4/zones", "https://api.example/outside", "https://api.example/client/v4-other/zones"} {
		req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, url, nil)
		if _, err := rt.RoundTrip(req); err == nil {
			t.Fatal("origin escaped", url)
		}
	}
	if calls != 0 {
		t.Fatal("invalid request reached network")
	}
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, "https://api.example/client/v4/zones", nil)
	resp, err := rt.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	rt.base = transportFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(io.MultiReader(strings.NewReader(`{"success":true,"result":"`), io.LimitReader(zeroReader{}, plan.MaxSnapshotBytes)))}, nil
	})
	if _, err = rt.RoundTrip(req); err == nil {
		t.Fatal("oversized body accepted")
	}
	idle := &idleTransport{}
	rt.base = idle
	client := &http.Client{Transport: rt}
	client.CloseIdleConnections()
	if !idle.closed.Load() {
		t.Fatal("idle pool not closed")
	}
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	req, _ = http.NewRequestWithContext(canceled, http.MethodGet, "https://api.example/client/v4/zones", nil)
	rt.base = transportFunc(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err = rt.RoundTrip(req); !errors.Is(err, context.Canceled) {
		t.Fatal("lost cancellation", err)
	}
}

type zeroReader struct{}

func (zeroReader) Read(b []byte) (int, error) { clear(b); return len(b), nil }

type budgetHost struct{ *fakeHost }

func (*budgetHost) Request(context.Context, string, string, bool, func(context.Context) error) error {
	return ledger.ErrBudget
}
func TestTransportSharedBudgetStopsNetworkAndRetryAfterBound(t *testing.T) {
	f := newFixture(t)
	rt := &requestTransport{host: &budgetHost{f.h}, origin: f.server.URL + "/client/v4/", token: testToken, base: transportFunc(func(*http.Request) (*http.Response, error) {
		t.Error("budget exceeded but sent network request")
		return nil, errors.New(testToken)
	})}
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodGet, f.server.URL+"/client/v4/zones", nil)
	_, err := rt.RoundTrip(req)
	var failure *host.Failure
	if !errors.As(err, &failure) || failure.Kind != host.FailureRateLimited || failure.RetryAfter != 5*time.Minute {
		t.Fatal(err)
	}
	for _, retry := range []string{"999999", "-1", "invalid", time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)} {
		err := statusFailure(429, retry)
		if !errors.As(err, &failure) || failure.RetryAfter < 0 || failure.RetryAfter > 15*time.Minute {
			t.Fatal(err)
		}
	}
}
