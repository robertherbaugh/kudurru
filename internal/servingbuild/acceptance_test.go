package servingbuild

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCompleteOwnersThroughNativeDNSAndHTTP(t *testing.T) {
	f := newFixture(t)
	for _, protocol := range []string{"udp", "tcp"} {
		t.Run(protocol, func(t *testing.T) {
			ready := make(chan struct{})
			server := &dns.Server{Handler: dnsserver.New("", nil, f.chain, nil), NotifyStartedFunc: func() { close(ready) }}
			var address string
			if protocol == "udp" {
				conn, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server.PacketConn, address = conn, conn.LocalAddr().String()
			} else {
				listener, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				server.Listener, address = listener, listener.Addr().String()
			}
			done := make(chan error, 1)
			go func() { done <- server.ActivateAndServe() }()
			t.Cleanup(func() {
				_ = server.Shutdown()
				if err := <-done; err != nil {
					t.Error(err)
				}
			})
			select {
			case <-ready:
			case <-time.After(time.Second):
				t.Fatal("DNS did not bind")
			}
			q := new(dns.Msg)
			q.SetQuestion("blocked.example.", dns.TypeA)
			answer, _, err := (&dns.Client{Net: protocol, Timeout: time.Second}).ExchangeContext(t.Context(), q, address)
			if err != nil {
				t.Fatal(err)
			}
			if len(answer.Answer) != 1 || answer.Answer[0].(*dns.A).A.String() != "192.0.2.11" {
				t.Fatal("native query missed complete owner cut", answer)
			}
		})
	}
	adapter, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(adapter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		principal, ok := auth.PrincipalFrom(r.Context())
		if !ok || principal.ID != "local:alice" {
			t.Error("HTTP lost captured identity")
		}
		w.WriteHeader(http.StatusNoContent)
	})))
	defer server.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/api/v1/settings", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	response, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		t.Fatal("HTTP authentication failed", response.StatusCode)
	}
}

func TestWholeBundleAfterActualFollowerInstallRestartAndPromotion(t *testing.T) {
	f := newFixture(t)
	image, err := f.store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "follower.db")
	var private []store.Kind
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: f.builder.opts.Schema.scheme, Validator: f.builder.opts.Schema.validator, InternalKinds: private, InitialMode: replication.Follower, Identity: "follower-b", ValidateReplicaEntry: security.ValidateEntry, ValidateReplicaImage: security.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: follower.Authority().Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	defer func() { _ = follower.Close() }()
	opts := f.builder.opts
	opts.Authority = follower.ServingAuthority
	b, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := serving.New(serving.Options{Source: follower, Required: serving.CompleteManifest(), Prepare: b.Prepare, RetryInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); _ = pub.Run(ctx) }()
	defer func() { cancel(); <-done }()
	select {
	case <-pub.Initialized():
	case <-time.After(10 * time.Second):
		t.Fatalf("replica publication failed: %+v", pub.Status())
	}
	held, err := pub.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	if !held.Bundle().Identity().Base.Same(installed) {
		t.Fatal("replica served unrelated image")
	}
	if _, err := f.auth(t, held).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}, f.now); err != nil {
		t.Fatal(err)
	}
	chain := newChain(t, pub)
	wantIP(t, query(t, chain, nil, "blocked.example."), "192.0.2.11")
	promoted, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "complete-owner-promotion"})
	if err != nil {
		t.Fatal(err)
	}
	if promoted.Term.Same(image.Term) {
		t.Fatal("promotion did not advance authority")
	}
	deadline := time.After(10 * time.Second)
	for {
		changed := pub.Subscribe()
		lease, err := pub.CaptureHTTP(t.Context())
		if err == nil {
			if !lease.Bundle().Identity().Base.Term.Same(promoted.Term) {
				t.Fatal("old term remained protected-ready")
			}
			lease.Release()
			break
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatal("promoted image was not published")
		}
	}
	wantIP(t, query(t, chain, held, "blocked.example."), "192.0.2.11")
	wantIP(t, query(t, chain, nil, "blocked.example."), "192.0.2.11")
}
