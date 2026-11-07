package servingbuild

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Test-only equivalent of root's canonical read adapter. It resolves settings
// from the same original auth owner and immutable startup host, never Store.
type policyReadConfig struct{ host *config.Config }

func (r policyReadConfig) Settings(ctx context.Context) (*config.Config, error) {
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, err
	}
	settings, err := view.Settings()
	if err != nil {
		return nil, err
	}
	doc := map[string]any{}
	for key, raw := range settings.Spec.Settings {
		var value any
		if err := json.Unmarshal(raw.Raw, &value); err != nil {
			return nil, err
		}
		doc[key] = value
	}
	cfg, _, err := config.ApplyClusterDocument(r.host, doc)
	return cfg, err
}
func (policyReadConfig) WriteSettings(context.Context, *config.Config, []string) error {
	return errors.New("test read-only settings")
}

type heldPolicyReads struct {
	*PolicyListReads
	remaining atomic.Int64
	entered   chan context.Context
	resume    chan struct{}
}

func (r *heldPolicyReads) PolicyListState(ctx context.Context, ns, name string, now time.Time) (int, bool, error) {
	if r.remaining.Add(-1) >= 0 {
		r.entered <- ctx
		select {
		case <-r.resume:
		case <-ctx.Done():
			return 0, false, ctx.Err()
		}
	}
	return r.PolicyListReads.PolicyListState(ctx, ns, name, now)
}

func policyReadHTTP(t *testing.T, f *fixture, p *serving.Publisher, read apihttp.PolicyListReadSource, now func() time.Time) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	a, err := servinghttp.New(servinghttp.Options{Publisher: p, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host,
		Authenticate: a.Middleware, Authorizer: a, CrossOrigin: cop, Now: now,
		ObjectReads: NewObjectReads(), CanonicalSettings: policyReadConfig{host: f.deps.Host}, PolicyListReads: read,
		Catalogue: apihttp.CatalogueSource{Read: NewCatalogueReads().Catalogue, Holder: catalogue.NewHolder(nil), Status: func() catalogue.Status { panic("live status consulted") }}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.Handler())
	t.Cleanup(s.Close)
	return s
}

func policyReadState(t *testing.T, response objectReadResponse) apihttp.PolicyState {
	t.Helper()
	body := diagnosticWant(t, response.diagnosticResponse, http.StatusOK)
	var state apihttp.PolicyState
	if err := json.Unmarshal([]byte(body), &state); err != nil {
		t.Fatal(err, body)
	}
	return state
}

func holdPolicyRead(t *testing.T, h *heldPolicyReads, server *httptest.Server, token string) (context.Context, <-chan objectReadResponse) {
	t.Helper()
	h.remaining.Store(1)
	done := make(chan objectReadResponse, 1)
	go func() { done <- objectReadGET(t.Context(), server, token, "policies/default/default/state") }()
	select {
	case ctx := <-h.entered:
		return ctx, done
	case <-time.After(5 * time.Second):
		t.Fatal("policy state did not reach original list reader")
		return nil, done
	}
}

func newHeldPolicyReads(t *testing.T) *heldPolicyReads {
	t.Helper()
	h := &heldPolicyReads{PolicyListReads: NewPolicyListReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	return h
}

func cleanupHeldPolicy(t *testing.T, h *heldPolicyReads) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-h.resume:
		default:
			close(h.resume)
		}
	})
}

func TestPolicyReadsActualSQLiteHeldObjectsConfigCatalogAndText(t *testing.T) {
	f := newCatalogFixture(t)
	a := catalogReadDocument(t, "alpha", f.url)
	f.holder.Set(a)
	var p public.Policy
	if err := f.store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.ClientGroupRefs, p.Spec.BlockListRefs = []string{"kids"}, []string{"deny", "missing"}
	p.Spec.Priority, p.Spec.BlockedServices, p.Spec.BlockedCategories = 10, []string{"alpha"}, []string{"video"}
	p.Spec.Schedule = &public.Schedule{Start: "00:00", End: "00:00"}
	kids := &public.ClientGroup{ObjectMeta: metav1.ObjectMeta{Name: "kids", Namespace: "default"}, Spec: public.ClientGroupSpec{Clients: []public.ClientMatcher{{IP: "192.0.2.1"}}}}
	peer := &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "higher", Namespace: "default"}, Spec: public.PolicySpec{Priority: 20, ClientGroupRefs: []string{"kids"}}}
	if err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, o := range []store.Object{&p, kids, peer} {
			if _, err := tx.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var list public.BlockList
	if err := f.store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "deny"}, &list); err != nil {
		t.Fatal(err)
	}
	list.Status.Entries = 999
	list.Status.Conditions = []metav1.Condition{{Type: public.ConditionReady, Status: metav1.ConditionFalse, Reason: "StaleReceipt", LastTransitionTime: metav1.NewTime(f.now)}}
	if err := f.store.UpdateStatus(t.Context(), &list); err != nil {
		t.Fatal(err)
	}
	catalogReadWait(t, f.publisher, a)
	h := newHeldPolicyReads(t)
	server := policyReadHTTP(t, f.fixture, f.publisher, h, func() time.Time { return f.now })
	cleanupHeldPolicy(t, h)
	original, done := holdPolicyRead(t, h, server, f.token)
	before, err := servinghttp.Identity(original)
	if err != nil {
		t.Fatal(err)
	}
	// One actual canonical transaction replaces policy/group/list/settings.
	if err := f.store.Command(t.Context(), f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, key := range []store.Key{{Kind: "Policy", Namespace: "default", Name: "default"}, {Kind: "Policy", Namespace: "default", Name: "higher"}, {Kind: "ClientGroup", Namespace: "default", Name: "kids"}, {Kind: "BlockList", Namespace: "default", Name: "deny"}, {Kind: "Kudurru", Name: "default"}} {
			var obj store.Object
			switch key.Kind {
			case "Policy":
				obj = &public.Policy{}
			case "ClientGroup":
				obj = &public.ClientGroup{}
			case "BlockList":
				obj = &public.BlockList{}
			case "Kudurru":
				obj = &public.Kudurru{}
			}
			if err := tx.Get(ctx, key, obj); err != nil {
				return err
			}
			switch o := obj.(type) {
			case *public.Policy:
				if o.Name == "default" {
					o.Spec.Priority, o.Spec.BlockedServices = 15, []string{"beta"}
				} else {
					o.Spec.Priority = 5
				}
			case *public.ClientGroup:
				o.Spec.Clients = append(o.Spec.Clients, public.ClientMatcher{IP: "192.0.2.2"})
			case *public.BlockList:
				o.Spec.Mode, o.Spec.Sources = "allow", []public.ListSource{{Inline: []string{"one.example", "two.example"}}}
			case *public.Kudurru:
				raw := o.Spec.Settings["blocking"]
				var document map[string]any
				if err := json.Unmarshal(raw.Raw, &document); err != nil {
					return err
				}
				document["defaultResponse"] = "zeroIP"
				var err error
				raw.Raw, err = json.Marshal(document)
				if err != nil {
					return err
				}
				o.Spec.Settings["blocking"] = raw
			}
			if _, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b := catalogReadDocument(t, "beta", f.url)
	f.holder.Set(b)
	after := catalogReadWait(t, f.publisher, b)
	if before.Base.Same(after.Base) {
		t.Fatal("fixture did not replace canonical sources")
	}
	close(h.resume)
	old := policyReadState(t, <-done)
	fresh := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if old.Priority != 10 || old.Groups[0].Clients != 1 || !slices.Equal(old.ShadowedBy, []string{"default/higher"}) || old.BlockMode != "customIP" || old.Services[0].Name != "alpha service" || old.SafeSearch.Engines[0].Name != "alpha search" || old.Categories[0].Name != "alpha category" || old.Lists[0].Entries != 1 || !old.Lists[0].Ready || old.Lists[0].Mode != "block" || old.Lists[1].Exists {
		t.Fatal("held state mixed live successor or stale receipt", old)
	}
	if fresh.Priority != 15 || fresh.Groups[0].Clients != 2 || len(fresh.ShadowedBy) != 0 || fresh.BlockMode != "zeroIP" || fresh.Services[0].Name != "beta service" || fresh.SafeSearch.Engines[0].Name != "beta search" || fresh.Categories[0].Name != "beta category" || fresh.Lists[0].Entries != 2 || !fresh.Lists[0].Ready || fresh.Lists[0].Mode != "allow" {
		t.Fatal("fresh state missed exact successor", fresh)
	}
	if _, _, err := h.PolicyListReads.PolicyListState(original, "default", "deny", f.now); err == nil {
		t.Fatal("completed policy request retained a usable lease")
	}
}

type policyRPZFixture struct {
	*fixture
	publisher *serving.Publisher
	keyrings  *KeyringOwner
	manager   *rpz.Manager
	clock     atomic.Int64
	stop      func()
}

func (f *policyRPZFixture) time() time.Time { return time.Unix(0, f.clock.Load()).UTC() }

func newPolicyRPZFixture(t *testing.T) *policyRPZFixture {
	t.Helper()
	keys := ring(t, "a")
	keyrings, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := &policyRPZFixture{fixture: newFixtureWithKeyrings(t, keyrings), keyrings: keyrings}
	f.clock.Store(f.now.UnixNano())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, dnsDone := make(chan struct{}), make(chan error, 1)
	server := &dns.Server{Listener: ln, TsigProvider: dnsserver.TSIGProvider(keys), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		if q.IsTsig() == nil || w.TsigStatus() != nil {
			t.Error("RPZ acquisition did not verify native TSIG")
			return
		}
		var records []dns.RR
		for _, raw := range []string{"policy.test. 30 IN SOA ns.policy.test. hostmaster.policy.test. 1 3600 10 60 5", "policy.test. 30 IN NS ns.policy.test.", "bad.policy.test. 30 IN CNAME .", "pass.policy.test. 30 IN CNAME rpz-passthru."} {
			rr, err := dns.NewRR(raw)
			if err != nil {
				t.Error(err)
				return
			}
			records = append(records, rr)
		}
		if q.Question[0].Qtype == dns.TypeSOA {
			records = records[:1]
		} else {
			records = append(records, dns.Copy(records[0]))
		}
		response := new(dns.Msg)
		response.SetReply(q)
		response.Authoritative, response.Answer = true, records
		response.SetTsig(q.IsTsig().Hdr.Name, q.IsTsig().Algorithm, 300, time.Now().Unix())
		if err := w.WriteMsg(response); err != nil {
			t.Error(err)
		}
	})}
	go func() { dnsDone <- server.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = server.Shutdown()
		if err := <-dnsDone; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("native RPZ listener did not start")
	}
	list := &public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "rpz", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{
		{Inline: []string{"text.example"}}, {Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{ln.Addr().String()}, TSIGKeyRef: "updates.example."}},
	}}}
	p := &public.Policy{}
	if err := f.store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "default"}, p); err != nil {
		t.Fatal(err)
	}
	p.Spec.BlockListRefs = []string{"rpz"}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, o := range []store.Object{list, p} {
			if _, err := tx.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	})
	cache, err := zonecache.New(f.store, zonecache.Options{Debounce: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	blobs, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "rpz.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	f.manager, err = rpz.New(rpz.Options{Cache: cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Keyring: keys,
		Receiver: &secondary.TransferClient{Keyring: keys}, PollInterval: time.Millisecond, Now: f.time, Timeout: time.Second, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	owner := NewTransferredRuntime(nil, nil, f.manager)
	opts := f.builder.opts
	opts.Now = f.time
	opts.Capture, err = owner.CaptureDependencies(opts.Capture)
	if err != nil {
		t.Fatal(err)
	}
	build, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	source, err := owner.GuardSource(f.store)
	if err != nil {
		t.Fatal(err)
	}
	source, err = keyrings.GuardSource(source)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher, err = serving.New(serving.Options{Source: source, Prepare: build.Prepare, Required: serving.CompleteManifest(), RetryInterval: time.Millisecond, DependencyChanges: keyrings.Changes})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	managerCtx, stopManager := context.WithCancel(ctx)
	managerDone := make(chan error, 1)
	go func() { managerDone <- f.manager.Run(managerCtx) }()
	var once sync.Once
	f.stop = func() {
		once.Do(func() {
			stopManager()
			if err := <-managerDone; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		})
	}
	done := make(chan error, 3)
	go func() { done <- cache.Start(ctx) }()
	go func() { done <- f.publisher.Run(ctx) }()
	go func() { done <- owner.Watch(ctx, f.publisher.Wake) }()
	t.Cleanup(func() {
		f.stop()
		cancel()
		for range 3 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		f.publisher.Close()
	})
	waitPolicyRPZ(t, f, 2, true)
	return f
}

func waitPolicyRPZ(t *testing.T, f *policyRPZFixture, rules int, available bool) {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		changed := f.publisher.Subscribe()
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err == nil {
			value, _ := lease.Bundle().Component("zones")
			s, err := value.(*servingzones.Prepared).SelectRPZ([]string{"default/rpz"})
			if err != nil {
				lease.Release()
				t.Fatal(err)
			}
			n, ready := s.Summary(f.time())
			lease.Release()
			if n == rules && ready == available {
				return
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("RPZ did not publish count%d/ready%v: %+v %+v", rules, available, f.manager.Statuses(), f.publisher.Status())
		}
	}
}

func TestPolicyReadsActualSQLiteNativeSignedRPZRotationAndExpiry(t *testing.T) {
	f := newPolicyRPZFixture(t)
	f.stop() // Keep the actual durable expiry; no concurrent successful renewal.
	h := newHeldPolicyReads(t)
	server := policyReadHTTP(t, f.fixture, f.publisher, h, f.time)
	cleanupHeldPolicy(t, h)
	oldCtx, done := holdPolicyRead(t, h, server, f.token)
	if n, ready, err := h.PolicyListReads.PolicyListState(oldCtx, "default", "rpz", f.time()); err != nil || n != 3 || !ready {
		t.Fatal("actual acquired RPZ plus text count", n, ready, err)
	}
	if _, _, err := h.PolicyListReads.PolicyListState(oldCtx, "other", "rpz", f.time()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("canonical namespace key borrowed another list", err)
	}
	if _, _, err := h.PolicyListReads.PolicyListState(oldCtx, "default", "rpz", time.Time{}); err == nil {
		t.Fatal("zero observation accepted")
	}
	if err := f.keyrings.Replace(t.Context(), ring(t, "b")); err != nil {
		t.Fatal(err)
	}
	waitPolicyRPZ(t, f, 0, false)
	close(h.resume)
	old := policyReadState(t, <-done)
	fresh := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if old.Lists[0].Entries != 3 || !old.Lists[0].Ready || fresh.Lists[0].Entries != 1 || fresh.Lists[0].Ready {
		t.Fatal("key rotation relabeled old acquired data or lost held cut", old.Lists, fresh.Lists)
	}
	// Existing durable fingerprint semantics permit identical restored bytes;
	// this is not a claim of fresh acquisition or an acquisition-epoch ABA fence.
	if err := f.keyrings.Replace(t.Context(), ring(t, "a")); err != nil {
		t.Fatal(err)
	}
	waitPolicyRPZ(t, f, 2, true)
	statuses := f.manager.Statuses()
	if len(statuses) != 1 || statuses[0].ExpiresAt.IsZero() {
		t.Fatal("acquired durable deadline missing", statuses)
	}
	f.clock.Store(statuses[0].ExpiresAt.UnixNano())
	expired := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if expired.Lists[0].Entries != 3 || expired.Lists[0].Ready {
		t.Fatal("exact expiry dropped raw diagnostic count or stayed ready", expired.Lists)
	}
	// A source edit changes identity and cannot borrow old RPZ even with key A.
	var list public.BlockList
	if err := f.store.Get(t.Context(), store.Key{Kind: "BlockList", Namespace: "default", Name: "rpz"}, &list); err != nil {
		t.Fatal(err)
	}
	list.Spec.Sources[1].RPZ.ZoneName = "replacement.test."
	if _, err := f.store.Apply(t.Context(), &list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	waitPolicyRPZ(t, f, 0, false)
	missing := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if missing.Lists[0].Entries != 1 || missing.Lists[0].Ready {
		t.Fatal("source mutation reused unrelated compiled feed", missing.Lists)
	}
	// An RPZ-only marker is compiled for policy selection, but it does not
	// constitute an available acquired source or contain executable rules.
	list.Spec.Sources = list.Spec.Sources[1:]
	if _, err := f.store.Apply(t.Context(), &list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	waitPolicyRPZ(t, f, 0, false)
	marker := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if marker.Lists[0].Entries != 0 || marker.Lists[0].Ready {
		t.Fatal("zero-entry RPZ marker fabricated readiness", marker.Lists)
	}
	list.Annotations = map[string]string{public.EnabledAnnotation: "false"}
	if _, err := f.store.Apply(t.Context(), &list, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	waitPolicyRPZ(t, f, 0, true) // Explicit disabled source is omitted, not missing.
	disabled := policyReadState(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state"))
	if disabled.Lists[0].Entries != 0 || disabled.Lists[0].Ready || disabled.Lists[0].Enabled {
		t.Fatal("disabled list reported serving readiness", disabled.Lists)
	}
}

func TestPolicyReadsActualSQLiteRejectsIncompleteOrMixedOriginalOwners(t *testing.T) {
	f := newFixture(t)
	for _, change := range []string{"missing-lists", "wrong-generation", "wrong-zones", "missing-primary"} {
		t.Run(change, func(t *testing.T) {
			p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
				prepared, err := f.builder.Prepare(ctx, c)
				if err != nil {
					return prepared, err
				}
				// Explicit test-only incoherent owners. Production builds these
				// parts together from one candidate, never from independent cuts.
				for i := range prepared.Components {
					switch prepared.Components[i].Name {
					case "objects":
						old := prepared.Components[i].Value.(*zonecache.Snapshot)
						cloned := *old
						switch change {
						case "wrong-generation":
							cloned.Generation++
						case "missing-lists":
							var raw []store.Object
							for _, kind := range old.Objects.Kinds() {
								raw = append(raw, old.Objects.List(kind)...)
							}
							next, err := zonecache.Prepare(zonecache.PrepareOptions{Kinds: zonecache.DefaultKinds(), Generation: old.Generation}, raw, nil)
							if err != nil {
								return prepared, err
							}
							cloned = *next
						}
						prepared.Components[i].Value = &cloned
					case "zones":
						if change == "wrong-zones" {
							prepared.Components[i].Value = &servingzones.Prepared{}
						}
					case "runtime":
						if change == "missing-primary" {
							cloned := *prepared.Components[i].Value.(*RuntimeOwner)
							cloned.primary = nil
							prepared.Components[i].Value = &cloned
						}
					}
				}
				return prepared, nil
			})
			server := policyReadHTTP(t, f, p, NewPolicyListReads(), func() time.Time { return f.now })
			diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "policies/default/default/state").diagnosticResponse, http.StatusServiceUnavailable)
		})
	}
	r := NewPolicyListReads()
	var missing *PolicyListReads
	if _, _, err := r.PolicyListState(context.Background(), "default", "deny", f.now); err == nil {
		t.Fatal("reader repaired absent original request")
	}
	if _, _, err := missing.PolicyListState(t.Context(), "default", "deny", f.now); err == nil {
		t.Fatal("nil reader accepted")
	}
}
