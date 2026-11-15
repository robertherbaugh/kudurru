package servingbuild

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	upstreamstage "github.com/robertherbaugh/kudurru/internal/stages/upstream"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

type upstreamReaderFunc func(context.Context) ([]upstream.ServerStatus, error)

func (f upstreamReaderFunc) UpstreamStates(ctx context.Context) ([]upstream.ServerStatus, error) {
	return f(ctx)
}

func upstreamReadHTTP(t *testing.T, f *fixture, p *serving.Publisher, read apihttp.UpstreamReadSource) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	auth, err := servinghttp.New(servinghttp.Options{Publisher: p, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host,
		Authenticate: auth.Middleware, Authorizer: auth, CrossOrigin: cop, UpstreamReads: read, NodeName: "held-node",
		Upstreams: apihttp.UpstreamStatusFunc(func() []upstream.ServerStatus { panic("complete upstream GET consulted live stage") })})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.Handler())
	t.Cleanup(s.Close)
	return s
}

func applyUpstreamReadRoutes(t *testing.T, f *fixture, name, address string) {
	t.Helper()
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var settings public.Kudurru
		if err := tx.Get(ctx, store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			return err
		}
		raw := settings.Spec.Settings["upstreams"]
		var err error
		raw.Raw, err = json.Marshal(map[string]any{"groups": map[string]any{"default": map[string]any{
			"servers": []string{name + "=" + address}, "strategy": "strict", "timeout": "1s",
		}}})
		if err != nil {
			return err
		}
		settings.Spec.Settings["upstreams"] = raw
		_, err = tx.Apply(ctx, &settings, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
}

func upstreamReadWait(t *testing.T, p *serving.Publisher, name string) *serving.Lease {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := p.Subscribe()
		lease, err := p.CaptureHTTP(t.Context())
		if err == nil {
			value, ok := lease.Bundle().Component("resolution")
			view, typed := value.(*upstreamstage.View)
			if ok && typed {
				status, err := view.Status()
				if err == nil && len(status) == 1 && status[0].Server == name {
					return lease
				}
			}
			lease.Release()
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("upstream routes did not publish: %+v", p.Status())
		}
	}
}

type upstreamReadServer struct {
	addr    string
	failing atomic.Bool
	calls   atomic.Int64
}

func startUpstreamReadServer(t *testing.T) *upstreamReadServer {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &upstreamReadServer{addr: pc.LocalAddr().String()}
	started := make(chan struct{})
	s := &dns.Server{PacketConn: pc, NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		f.calls.Add(1)
		m := new(dns.Msg)
		m.SetReply(q)
		if f.failing.Load() {
			m.Rcode = dns.RcodeRefused
		} else {
			m.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = s.ActivateAndServe() }()
	<-started
	t.Cleanup(func() { _ = s.Shutdown() })
	return f
}

func upstreamReadBody(t *testing.T, response objectReadResponse) apihttp.UpstreamsBody {
	t.Helper()
	body := diagnosticWant(t, response.diagnosticResponse, http.StatusOK)
	var out apihttp.UpstreamsBody
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err, body)
	}
	return out
}

func TestCapturedUpstreamActualSQLiteHeldRoutesAndChangingCounters(t *testing.T) {
	f := newFixture(t)
	a, b := startUpstreamReadServer(t), startUpstreamReadServer(t)
	applyUpstreamReadRoutes(t, f, "alpha", a.addr)
	held := upstreamReadWait(t, f.pub, "alpha")
	defer held.Release()
	r := NewUpstreamReads()
	entered, resume := make(chan context.Context, 1), make(chan struct{})
	var hold atomic.Bool
	hold.Store(true)
	server := upstreamReadHTTP(t, f, f.pub, upstreamReaderFunc(func(ctx context.Context) ([]upstream.ServerStatus, error) {
		if hold.CompareAndSwap(true, false) {
			entered <- ctx
			select {
			case <-resume:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return r.UpstreamStates(ctx)
	}))
	t.Cleanup(func() {
		select {
		case <-resume:
		default:
			close(resume)
		}
	})
	done := make(chan objectReadResponse, 1)
	go func() { done <- objectReadGET(t.Context(), server, f.token, "stats/upstreams") }()
	var original context.Context
	select {
	case original = <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream read did not hold original authenticated request")
	}
	identity, err := servinghttp.Identity(original)
	if err != nil || identity != held.Bundle().Identity() {
		t.Fatal("DNS and HTTP fixtures did not retain the exact same original installation", identity, err)
	}
	before, err := r.UpstreamStates(original)
	if err != nil || len(before) != 1 || before[0].Server != "alpha" || !before[0].Healthy {
		t.Fatal("initial retained upstream status", before, err)
	}
	applyUpstreamReadRoutes(t, f, "beta", b.addr)
	fresh := upstreamReadWait(t, f.pub, "beta")
	defer fresh.Release()
	if identity.Base.Same(fresh.Bundle().Identity().Base) {
		t.Fatal("canonical route replacement did not advance source generation")
	}
	// Native exchanges deliberately change health on the retired but retained
	// Set after B publishes. Status must sample A's counters, not freeze them.
	response := query(t, f.chain, held, "healthy-counter.example.")
	if response.Msg.Rcode != dns.RcodeSuccess {
		t.Fatal("held retired Set was released before its lease", response.Msg)
	}
	response.Close()
	a.failing.Store(true)
	for i := range 3 {
		response := query(t, f.chain, held, fmt.Sprintf("failed-counter-%d.example.", i))
		if response.Msg.Rcode != dns.RcodeServerFailure {
			t.Fatal("native retained-route failure was not observed", response.Msg)
		}
		response.Close()
	}
	after, err := r.UpstreamStates(original)
	if err != nil || len(after) != 1 || after[0].Server != "alpha" || after[0].Healthy || after[0].ConsecutiveFailures != 3 || after[0].BackoffUntil.IsZero() || after[0].RTT <= 0 || !before[0].Healthy || before[0].ConsecutiveFailures != 0 {
		t.Fatal("held membership/counter samples mixed or aliased", before, after, err)
	}
	after[0].Server = "mutated"
	freshBody := upstreamReadBody(t, objectReadGET(t.Context(), server, f.token, "stats/upstreams"))
	if len(freshBody.Servers) != 1 || freshBody.Servers[0].Server != "beta" || !freshBody.Servers[0].Healthy || freshBody.Servers[0].ConsecutiveFailures != 0 || freshBody.Servers[0].Node != "held-node" || len(freshBody.Items) != 0 {
		t.Fatal("fresh status used old/live routes or fabricated traffic history", freshBody)
	}
	close(resume)
	heldBody := upstreamReadBody(t, <-done)
	if len(heldBody.Servers) != 1 || heldBody.Servers[0].Server != "alpha" || heldBody.Servers[0].Healthy || heldBody.Servers[0].ConsecutiveFailures != 3 || heldBody.Servers[0].BackoffUntil == nil || a.calls.Load() != 4 || b.calls.Load() != 0 {
		t.Fatal("held HTTP status did not use its retained Set's current counters", heldBody, a.calls.Load(), b.calls.Load())
	}
	if _, err := r.UpstreamStates(original); err == nil {
		t.Fatal("released HTTP context still exposed retained Set")
	}
}

func TestCapturedUpstreamActualSQLiteUnavailableOriginalOwner(t *testing.T) {
	f := newFixture(t)
	r := NewUpstreamReads()
	var nilReader *UpstreamReads
	for _, read := range []upstreamReaderFunc{
		nilReader.UpstreamStates,
		func(context.Context) ([]upstream.ServerStatus, error) { return r.UpstreamStates(nil) }, //nolint:staticcheck // Deliberate missing original context.
		func(context.Context) ([]upstream.ServerStatus, error) { return r.UpstreamStates(context.Background()) },
		func(ctx context.Context) ([]upstream.ServerStatus, error) {
			canceled, cancel := context.WithCancel(ctx)
			cancel()
			return r.UpstreamStates(canceled)
		},
	} {
		server := upstreamReadHTTP(t, f, f.pub, read)
		diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "stats/upstreams").diagnosticResponse, http.StatusServiceUnavailable)
	}
	for _, value := range []any{struct{}{}, &upstreamstage.View{}} {
		// Explicit test-only malformed component, never a production issuer.
		p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
			prepared, err := f.builder.Prepare(ctx, c)
			for i := range prepared.Components {
				if prepared.Components[i].Name == "resolution" {
					prepared.Components[i].Value = value
				}
			}
			return prepared, err
		})
		server := upstreamReadHTTP(t, f, p, r)
		diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "stats/upstreams").diagnosticResponse, http.StatusServiceUnavailable)
	}
}
