package servingbuild

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	cachedata "github.com/robertherbaugh/kudurru/internal/cache"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/blocking"
	"github.com/robertherbaugh/kudurru/internal/stages/cache"
	"github.com/robertherbaugh/kudurru/internal/stages/control"
	"github.com/robertherbaugh/kudurru/internal/stages/identify"
	"github.com/robertherbaugh/kudurru/internal/stages/postprocess"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The barrier runs after native HTTP authentication/authorization but before
// cloning the DNS lease. Each held request must keep precisely that HTTP cut.
type heldQueryCapture struct {
	remaining atomic.Int64
	entered   chan serving.Identity
	resume    chan struct{}
	clones    chan *serving.Lease
}

func newHeldQueryCapture(t *testing.T) *heldQueryCapture {
	t.Helper()
	h := &heldQueryCapture{entered: make(chan serving.Identity, 1), resume: make(chan struct{}), clones: make(chan *serving.Lease, 8)}
	h.remaining.Store(1)
	t.Cleanup(func() {
		select {
		case <-h.resume:
		default:
			close(h.resume)
		}
	})
	return h
}

func (h *heldQueryCapture) capture(req *pipeline.Request) (pipeline.ServingLease, error) {
	if h.remaining.Add(-1) >= 0 {
		identity, err := servinghttp.Identity(req.Ctx)
		if err != nil {
			return nil, err
		}
		h.entered <- identity
		select {
		case <-h.resume:
		case <-req.Ctx.Done():
			return nil, req.Ctx.Err()
		}
	}
	// Exact production bridge: no publisher/current check or live recapture.
	lease, err := servinghttp.CloneLease(req.Ctx)
	if lease != nil {
		h.clones <- lease // borrowed only to assert release after HTTP returns
	}
	return servingdns.Wrap(lease), err
}

type queryIdentityStage struct{ identities chan serving.Identity }

func (queryIdentityStage) Name() string { return "ingress" }
func (s queryIdentityStage) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	original, err := servinghttp.Identity(req.Ctx)
	if err != nil {
		return nil, err
	}
	bundle, ok := req.Serving.Value().(*serving.Bundle)
	if !ok || bundle.Identity() != original || !req.Client.Synthetic || req.Client.TSIGKey != "" || req.Msg.IsTsig() != nil {
		return nil, pipeline.ErrServingCapture
	}
	s.identities <- bundle.Identity()
	return next(req)
}

func queryCaptureChain(t *testing.T, f *fixture, now func() time.Time) (*pipeline.Chain, <-chan serving.Identity) {
	t.Helper()
	identities := make(chan serving.Identity, 16)
	i, a, u := identify.New(nil, nil), authoritative.New(nil), upstream.NewStage(nil)
	a.Now = now
	b, err := blocking.New(blocking.Options{})
	if err != nil {
		t.Fatal(err)
	}
	c := cache.New(cachedata.New(cachedata.Options{}), cache.Settings{}, u)
	r := &postprocess.Stage{}
	ctl := control.New(control.Options{})
	chain := pipeline.MustNew(queryIdentityStage{identities}, i, ctl, r, a, b, c, u)
	if err := BindDNS(f.pub, DNSStages{Chain: chain, Control: ctl, Identify: i, Authority: a, Blocking: b, Cache: c, Response: r, Resolution: u}, now); err != nil {
		t.Fatal(err)
	}
	chain.SetCapture(func(*pipeline.Request) (pipeline.ServingLease, error) {
		t.Error("synthetic query recaptured the live DNS publisher")
		return nil, pipeline.ErrServingCapture
	})
	return chain, identities
}

func queryCaptureHTTP(t *testing.T, f *fixture, chain *pipeline.Chain, capture pipeline.Capture) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host, Authenticate: auth.Middleware, Authorizer: auth, CrossOrigin: cop, ObjectReads: NewObjectReads(), Chain: chain, DNSQueryCapture: capture})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api.Handler())
	t.Cleanup(server.Close)
	return server
}

func queryCapturePOST(ctx context.Context, server *httptest.Server, token, name string) diagnosticResponse {
	body, err := json.Marshal(apihttp.DNSQueryRequest{Name: name})
	if err != nil {
		return diagnosticResponse{err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/api/v1/dns/query", strings.NewReader(string(body)))
	if err != nil {
		return diagnosticResponse{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	response, err := server.Client().Do(req)
	if err != nil {
		return diagnosticResponse{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(response.Body)
	return diagnosticResponse{code: response.StatusCode, body: string(data), err: err}
}

func queryCaptureResult(t *testing.T, response diagnosticResponse) apihttp.DNSQueryResult {
	t.Helper()
	body := diagnosticWant(t, response, http.StatusOK)
	var result apihttp.DNSQueryResult
	if err := json.Unmarshal([]byte(body), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func queryCaptureEntered(t *testing.T, h *heldQueryCapture) serving.Identity {
	t.Helper()
	select {
	case id := <-h.entered:
		return id
	case <-time.After(5 * time.Second):
		t.Fatal("original HTTP request did not reach DNS capture")
		return serving.Identity{}
	}
}

func queryCaptureReceived(t *testing.T, done <-chan diagnosticResponse) diagnosticResponse {
	t.Helper()
	select {
	case response := <-done:
		return response
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP DNS query did not finish")
		return diagnosticResponse{}
	}
}

func queryCaptureReleased(t *testing.T, h *heldQueryCapture, count int) {
	t.Helper()
	for range count {
		select {
		case clone := <-h.clones:
			if clone.Bundle() != nil {
				t.Fatal("query retained its cloned lease after response")
			}
		default:
			t.Fatal("query did not clone original lease")
		}
	}
}

func TestDNSQueryCaptureActualSQLiteHeldHTTPAndFreshSettings(t *testing.T) {
	f := newFixture(t)
	chain, identities := queryCaptureChain(t, f, nil)
	h := newHeldQueryCapture(t)
	server := queryCaptureHTTP(t, f, chain, h.capture)
	done := make(chan diagnosticResponse, 1)
	go func() { done <- queryCapturePOST(t.Context(), server, f.token, "blocked.example.") }()
	original := queryCaptureEntered(t, h)
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, settings("192.0.2.22"), store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	f.waitCurrent(t)
	fresh := queryCaptureResult(t, queryCapturePOST(t.Context(), server, f.token, "blocked.example."))
	freshID := <-identities
	close(h.resume)
	held := queryCaptureResult(t, queryCaptureReceived(t, done))
	heldID := <-identities
	if held.Rcode != "NOERROR" || fresh.Rcode != "NOERROR" || len(held.Answer) != 1 || len(fresh.Answer) != 1 || !strings.Contains(held.Answer[0], "192.0.2.11") || !strings.Contains(fresh.Answer[0], "192.0.2.22") || heldID != original || freshID.Base.Same(original.Base) {
		t.Fatal("HTTP query mixed cuts", held, fresh, original, heldID, freshID)
	}
	queryCaptureReleased(t, h, 2)
}

func TestDNSQueryCaptureActualSameBaseResourceReplacement(t *testing.T) {
	keys, err := NewKeyringOwner(ring(t, "a"))
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keys)
	chain, identities := queryCaptureChain(t, f, nil)
	h := newHeldQueryCapture(t)
	server := queryCaptureHTTP(t, f, chain, h.capture)
	done := make(chan diagnosticResponse, 1)
	go func() { done <- queryCapturePOST(t.Context(), server, f.token, "blocked.example.") }()
	original := queryCaptureEntered(t, h)
	if err := keys.Replace(t.Context(), ring(t, "b")); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	fresh := queryCaptureResult(t, queryCapturePOST(t.Context(), server, f.token, "blocked.example."))
	freshID := <-identities
	close(h.resume)
	held := queryCaptureResult(t, queryCaptureReceived(t, done))
	heldID := <-identities
	if held.Rcode != "NOERROR" || fresh.Rcode != "NOERROR" || heldID != original || !freshID.Base.Same(original.Base) || freshID.Dependencies == original.Dependencies || freshID.Publication == original.Publication {
		t.Fatal("same-Base resource replacement upgraded original cut", original, freshID, heldID)
	}
	queryCaptureReleased(t, h, 2)
}

func TestDNSQueryCaptureActualHTTPMissingOrCanceledOriginalUnavailable(t *testing.T) {
	f := newFixture(t)
	chain, identities := queryCaptureChain(t, f, nil)
	for _, kind := range []string{"missing", "canceled"} {
		t.Run(kind, func(t *testing.T) {
			server := queryCaptureHTTP(t, f, chain, func(req *pipeline.Request) (pipeline.ServingLease, error) {
				ctx := context.Background()
				if kind == "canceled" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithCancel(req.Ctx)
					cancel()
				}
				lease, err := servinghttp.CloneLease(ctx)
				return servingdns.Wrap(lease), err
			})
			diagnosticWant(t, queryCapturePOST(t.Context(), server, f.token, "blocked.example."), http.StatusServiceUnavailable)
			select {
			case <-identities:
				t.Fatal("unavailable original reached DNS stages")
			default:
			}
		})
	}
}

func TestDNSQueryCaptureActualCommittedCNAMEUDPAndAbsoluteExpiry(t *testing.T) {
	f := newFixture(t)
	r, _, ctx := primaryFixture(t, f)
	expiry := f.now.Add(time.Hour).Truncate(time.Second)
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "aliases", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "uncommitted"}, Endpoints: []public.Endpoint{
			{DNSName: "alias.uncommitted.example.", RecordType: "CNAME", Targets: []string{"outside.test."}},
			{DNSName: "lease.uncommitted.example.", RecordType: "A", Targets: []string{"192.0.2.5"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expiry}}},
		}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	_, lifecycle := beginPrimary(ctx, t, f, r)
	view, err := r.Reconcile(ctx, lifecycle.Spec.Zone)
	if err != nil || view == nil || view.Head == nil || view.Head.Spec.CommitSequence == 0 {
		t.Fatal("no committed primary head", err)
	}
	alpha, beta := startUpstreamReadServer(t), startUpstreamReadServer(t)
	applyUpstreamReadRoutes(t, f, "alpha", alpha.addr)
	lease := upstreamReadWait(t, f.pub, "alpha")
	lease.Release()
	var clock atomic.Int64
	clock.Store(expiry.Add(-time.Nanosecond).UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	chain, identities := queryCaptureChain(t, f, now)
	h := newHeldQueryCapture(t)
	server := queryCaptureHTTP(t, f, chain, h.capture)
	done := make(chan diagnosticResponse, 1)
	go func() { done <- queryCapturePOST(t.Context(), server, f.token, "alias.uncommitted.example.") }()
	original := queryCaptureEntered(t, h)
	applyUpstreamReadRoutes(t, f, "beta", beta.addr)
	lease = upstreamReadWait(t, f.pub, "beta")
	lease.Release()
	fresh := queryCaptureResult(t, queryCapturePOST(t.Context(), server, f.token, "alias.uncommitted.example."))
	<-identities
	close(h.resume)
	held := queryCaptureResult(t, queryCaptureReceived(t, done))
	heldID := <-identities
	if held.Rcode != "NOERROR" || fresh.Rcode != "NOERROR" || held.Upstream != "default/alpha" || fresh.Upstream != "default/beta" || len(held.Answer) != 2 || len(fresh.Answer) != 2 || !strings.Contains(held.Answer[0], "CNAME") || heldID != original || alpha.calls.Load() != 1 || beta.calls.Load() != 1 {
		t.Fatal("CNAME subquery lost captured route", held, fresh, alpha.calls.Load(), beta.calls.Load())
	}
	queryCaptureReleased(t, h, 2)
	// Retain another original HTTP cut across the exact expiry. A held lease
	// preserves its owners, never historical permission to serve expired data.
	expiring := newHeldQueryCapture(t)
	expiryServer := queryCaptureHTTP(t, f, chain, expiring.capture)
	go func() { done <- queryCapturePOST(t.Context(), expiryServer, f.token, "alias.uncommitted.example.") }()
	queryCaptureEntered(t, expiring)
	clock.Store(expiry.UnixNano())
	close(expiring.resume)
	expired := queryCaptureResult(t, queryCaptureReceived(t, done))
	<-identities
	if expired.Rcode != "SERVFAIL" || len(expired.Answer) != 0 || alpha.calls.Load() != 1 || beta.calls.Load() != 1 {
		t.Fatal("held HTTP revived expired primary or reached network", expired)
	}
	queryCaptureReleased(t, expiring, 1)
}
