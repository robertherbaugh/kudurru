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
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/rpz"
	"github.com/robertherbaugh/kudurru/internal/secondary"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/servingzones"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/localdb"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/stub"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type heldInfoReads struct {
	*InfoMessageReads
	hold    atomic.Bool
	entered chan context.Context
	resume  chan struct{}
}

func (h *heldInfoReads) InfoMessages(ctx context.Context, now time.Time, emit func(string, string, string, string, string, string) error) error {
	if h.hold.CompareAndSwap(true, false) {
		h.entered <- ctx
		select {
		case <-h.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return h.InfoMessageReads.InfoMessages(ctx, now, emit)
}

func infoReadHTTP(t *testing.T, f *fixture, p *serving.Publisher, read apihttp.InfoMessageReadSource, now func() time.Time) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	a, err := servinghttp.New(servinghttp.Options{Publisher: p, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	api, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host,
		Authenticate: a.Middleware, Authorizer: a, CrossOrigin: cop, Now: now, InfoMessageReads: read})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(api.Handler())
	t.Cleanup(s.Close)
	return s
}

func infoReadMessages(t *testing.T, response objectReadResponse) []apihttp.DiagnosisMessage {
	t.Helper()
	body := diagnosticWant(t, response.diagnosticResponse, http.StatusOK)
	var out apihttp.List[apihttp.DiagnosisMessage]
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err, body)
	}
	if out.Items == nil {
		t.Fatal("nil messages", body)
	}
	return out.Items
}

func infoReadHas(messages []apihttp.DiagnosisMessage, source, object string) bool {
	return slices.ContainsFunc(messages, func(m apihttp.DiagnosisMessage) bool { return m.Source == source && m.Object == object })
}

func infoReadOriginal(ctx context.Context, t *testing.T, reader *InfoMessageReads, now time.Time) []apihttp.DiagnosisMessage {
	t.Helper()
	var out []apihttp.DiagnosisMessage
	if err := reader.InfoMessages(ctx, now, func(severity, source, reason, object, message, hint string) error {
		out = append(out, apihttp.DiagnosisMessage{Severity: severity, Source: source, Reason: reason, Object: object, Message: message, Hint: hint})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func holdInfoRead(t *testing.T, h *heldInfoReads, server *httptest.Server, token string) (context.Context, <-chan objectReadResponse) {
	t.Helper()
	h.hold.Store(true)
	done := make(chan objectReadResponse, 1)
	go func() { done <- objectReadGET(t.Context(), server, token, "info/messages") }()
	t.Cleanup(func() {
		select {
		case <-h.resume:
		default:
			close(h.resume)
		}
	})
	select {
	case ctx := <-h.entered:
		return ctx, done
	case <-time.After(5 * time.Second):
		t.Fatal("diagnosis request did not retain original authenticated lease")
		return nil, done
	}
}

// The actual managers acquire through native local TCP/TSIG using production
// TransferClient and stub.Client. No status/model injection or live provider.
type infoTransferFixture struct {
	*fixture
	publisher    *serving.Publisher
	keys         *KeyringOwner
	secondary    *secondary.Manager
	stub         *stub.Manager
	rpz          *rpz.Manager
	clock        atomic.Int64
	stopManagers func()
	axfr         atomic.Int32
	discovery    atomic.Int32
}

func (f *infoTransferFixture) time() time.Time { return time.Unix(0, f.clock.Load()).UTC() }

func newInfoTransferFixture(t *testing.T) *infoTransferFixture {
	t.Helper()
	keys := ring(t, "a")
	keyrings, err := NewKeyringOwner(keys)
	if err != nil {
		t.Fatal(err)
	}
	f := &infoTransferFixture{fixture: newFixtureWithKeyrings(t, keyrings), keys: keyrings}
	f.clock.Store(f.now.UnixNano())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started, dnsDone := make(chan struct{}), make(chan error, 1)
	dnsServer := &dns.Server{Listener: ln, TsigProvider: dnsserver.TSIGProvider(keys), NotifyStartedFunc: func() { close(started) }, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, q *dns.Msg) {
		defer func() { _ = w.Close() }()
		apex := q.Question[0].Name
		records := []dns.RR{
			&dns.SOA{Hdr: dns.RR_Header{Name: apex, Rrtype: dns.TypeSOA, Class: dns.ClassINET, Ttl: 30}, Ns: "ns." + apex, Mbox: "hostmaster." + apex, Serial: 42, Refresh: 3600, Retry: 10, Expire: 60, Minttl: 5},
			&dns.NS{Hdr: dns.RR_Header{Name: apex, Rrtype: dns.TypeNS, Class: dns.ClassINET, Ttl: 30}, Ns: "ns." + apex},
			&dns.A{Hdr: dns.RR_Header{Name: "ns." + apex, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 30}, A: net.IPv4(127, 0, 0, 1).To4()},
		}
		response := new(dns.Msg)
		response.SetReply(q)
		response.Authoritative = true
		switch q.Question[0].Qtype {
		case dns.TypeSOA:
			response.Answer = records[:1]
		case dns.TypeNS:
			f.discovery.Add(1)
			response.Answer, response.Extra = records[1:2], records[2:]
		case dns.TypeAXFR:
			if q.IsTsig() == nil || w.TsigStatus() != nil {
				t.Error("native transfer lacked verified TSIG")
				return
			}
			f.axfr.Add(1)
			if apex == "policy.test." {
				records = append(records, &dns.CNAME{Hdr: dns.RR_Header{Name: "bad." + apex, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: 30}, Target: "."})
			}
			response.Answer = append(records, dns.Copy(records[0]))
		default:
			response.Rcode = dns.RcodeNameError
		}
		if tsig := q.IsTsig(); tsig != nil {
			if w.TsigStatus() != nil {
				t.Error("native request TSIG did not verify")
				return
			}
			response.SetTsig(tsig.Hdr.Name, tsig.Algorithm, 300, time.Now().Unix())
		}
		if err := w.WriteMsg(response); err != nil {
			t.Error(err)
		}
	})}
	go func() { dnsDone <- dnsServer.ActivateAndServe() }()
	t.Cleanup(func() {
		_ = dnsServer.Shutdown()
		if err := <-dnsDone; err != nil {
			t.Error(err)
		}
	})
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("native TCP listener did not start")
	}
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, o := range []store.Object{
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: public.DNSZoneSpec{ZoneName: ".", Type: public.ZoneTypeForward, Forward: &public.ForwardSpec{UpstreamGroup: "default"}}},
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "secondary"}, Spec: public.DNSZoneSpec{ZoneName: "secondary.test.", Type: public.ZoneTypeSecondary, Secondary: &public.SecondarySpec{Primaries: []string{ln.Addr().String()}, TSIGKeyRef: "updates.example."}}},
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "stub"}, Spec: public.DNSZoneSpec{ZoneName: "stub.test.", Type: public.ZoneTypeStub, Stub: &public.StubSpec{Nameservers: []string{"tcp://" + ln.Addr().String()}}}},
			&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "rpz", Namespace: "default"}, Spec: public.BlockListSpec{Mode: "block", Sources: []public.ListSource{{Format: "rpz", RPZ: &public.RPZSource{ZoneName: "policy.test.", Primaries: []string{ln.Addr().String()}, TSIGKeyRef: "updates.example."}}}}},
		} {
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
	blobs, err := localdb.OpenSnapshotStore(t.Context(), filepath.Join(t.TempDir(), "acquired.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blobs.Close() })
	f.secondary, err = secondary.New(secondary.Options{Cache: cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Keyring: keys, Receiver: &secondary.TransferClient{Keyring: keys, Timeout: time.Second}, Now: f.time, PollInterval: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.stub, err = stub.New(stub.Options{Cache: cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Discoverer: &stub.Client{Now: f.time, Timeout: time.Second, QueryTimeout: time.Second}, Now: f.time, PollInterval: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	f.rpz, err = rpz.New(rpz.Options{Cache: cache, Blobs: blobs, ClusterID: f.store.Info().ClusterID, Keyring: keys, Receiver: &secondary.TransferClient{Keyring: keys, Timeout: time.Second}, Now: f.time, Timeout: time.Second, PollInterval: time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	owner := NewTransferredRuntime(f.secondary, f.stub, f.rpz)
	opts := f.builder.opts
	opts.Now = f.time
	opts.Capture, err = owner.CaptureDependencies(opts.Capture)
	if err != nil {
		t.Fatal(err)
	}
	builder, err := New(opts)
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
	f.publisher, err = serving.New(serving.Options{Source: source, Required: serving.CompleteManifest(), Prepare: builder.Prepare, RetryInterval: time.Millisecond, DependencyChanges: keyrings.Changes})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	managerCtx, stopManagers := context.WithCancel(ctx)
	managers := make(chan error, 3)
	go func() { managers <- f.secondary.Run(managerCtx) }()
	go func() { managers <- f.stub.Run(managerCtx) }()
	go func() { managers <- f.rpz.Run(managerCtx) }()
	var once sync.Once
	f.stopManagers = func() {
		once.Do(func() {
			stopManagers()
			for range 3 {
				if err := <-managers; err != nil && !errors.Is(err, context.Canceled) {
					t.Error(err)
				}
			}
		})
	}
	done := make(chan error, 3)
	go func() { done <- cache.Start(ctx) }()
	go func() { done <- f.publisher.Run(ctx) }()
	go func() { done <- owner.Watch(ctx, f.publisher.Wake) }()
	t.Cleanup(func() {
		f.stopManagers()
		cancel()
		for range 3 {
			if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		}
		f.publisher.Close()
	})
	infoWaitTransferred(t, f, true, true, true)
	if f.axfr.Load() < 2 || f.discovery.Load() == 0 {
		t.Fatal("fixture did not perform real acquisition", f.axfr.Load(), f.discovery.Load())
	}
	f.stopManagers() // Fixed actual durable deadlines, never renewed by tests.
	return f
}

func infoWaitTransferred(t *testing.T, f *infoTransferFixture, secondaryReady, stubReady, rpzReady bool) {
	t.Helper()
	image, _, err := f.store.ReadServingCandidate(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	target := image.Generation()
	deadline := time.After(15 * time.Second)
	for {
		changed := f.publisher.Subscribe()
		lease, err := f.publisher.CaptureHTTP(t.Context())
		if err == nil {
			baseMatches := lease.Bundle().Identity().Base.Same(target)
			value, _ := lease.Bundle().Component("zones")
			p := value.(*servingzones.Prepared)
			set := p.Zones(f.time())
			s, st := set.Match("secondary.test."), set.Match("stub.test.")
			r, selectErr := p.SelectRPZ([]string{"default/rpz"})
			readyRPZ := false
			if selectErr == nil {
				_, readyRPZ = r.Summary(f.time())
			}
			readySecondary := s != nil && s.Name() == "secondary.test." && s.Type() == public.ZoneTypeSecondary && s.RoutingAvailableAt(f.time())
			readyStub := st != nil && st.Name() == "stub.test." && st.Type() == public.ZoneTypeStub && st.RoutingAvailableAt(f.time())
			ready := baseMatches && readySecondary == secondaryReady && readyStub == stubReady && readyRPZ == rpzReady
			lease.Release()
			if ready {
				return
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("acquired runtime publication mismatch: %+v sec=%+v stub=%+v rpz=%+v", f.publisher.Status(), f.secondary.Statuses(), f.stub.Statuses(), f.rpz.Statuses())
		}
	}
}

func TestInfoMessageReadsActualSQLiteNativeAcquisitionHeldExpiryRotationAndSourceChange(t *testing.T) {
	f := newInfoTransferFixture(t)
	h := &heldInfoReads{InfoMessageReads: NewInfoMessageReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	server := infoReadHTTP(t, f.fixture, f.publisher, h, f.time)
	original, done := holdInfoRead(t, h, server, f.token)
	before := infoReadOriginal(original, t, h.InfoMessageReads, f.time())
	if infoReadHas(before, "secondary", "DNSZone/secondary") || infoReadHas(before, "stub", "DNSZone/stub") || infoReadHas(before, "blocklists", "BlockList/default/rpz") {
		t.Fatal("real acquired data reported unavailable", before)
	}
	zones, err := servinghttp.Component[*servingzones.Prepared](original, "zones")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"secondary", "stub"} {
		model := zones.Zones(f.time()).Match(name + ".test.")
		deadline := model.NextExpiry()
		if deadline.IsZero() {
			t.Fatal("real acquired copy missing deadline", name)
		}
		beforeExpiry := infoReadOriginal(original, t, h.InfoMessageReads, deadline.Add(-time.Nanosecond))
		atExpiry := infoReadOriginal(original, t, h.InfoMessageReads, deadline)
		if infoReadHas(beforeExpiry, name, "DNSZone/"+name) || !infoReadHas(atExpiry, name, "DNSZone/"+name) {
			t.Fatal("held exact expiry mismatch", name, beforeExpiry, atExpiry)
		}
	}
	rpzStatus := f.rpz.Statuses()
	if len(rpzStatus) != 1 || rpzStatus[0].ExpiresAt.IsZero() {
		t.Fatal("real RPZ expiry missing", rpzStatus)
	}
	if infoReadHas(infoReadOriginal(original, t, h.InfoMessageReads, rpzStatus[0].ExpiresAt.Add(-time.Nanosecond)), "blocklists", "BlockList/default/rpz") || !infoReadHas(infoReadOriginal(original, t, h.InfoMessageReads, rpzStatus[0].ExpiresAt), "blocklists", "BlockList/default/rpz") {
		t.Fatal("RPZ exact expiry fabricated readiness")
	}
	// Actual original lease and held acquired data remain valid after key rotation.
	if err := f.keys.Replace(t.Context(), ring(t, "b")); err != nil {
		t.Fatal(err)
	}
	infoWaitTransferred(t, f, false, true, false)
	fresh := infoReadMessages(t, objectReadGET(t.Context(), server, f.token, "info/messages"))
	if !infoReadHas(fresh, "secondary", "DNSZone/secondary") || !infoReadHas(fresh, "blocklists", "BlockList/default/rpz") || infoReadHas(fresh, "stub", "DNSZone/stub") {
		t.Fatal("new cut borrowed old acquired credentials or lost independent stub", fresh)
	}
	if got := infoReadOriginal(original, t, h.InfoMessageReads, f.time()); !slices.Equal(got, before) {
		t.Fatal("rotation invalidated or mutated held read", got, before)
	}
	// Restore identical key bytes under the documented durable equality
	// contract before editing sources, so identity rejection is independent
	// from the credential mismatch above.
	if err := f.keys.Replace(t.Context(), ring(t, "a")); err != nil {
		t.Fatal(err)
	}
	infoWaitTransferred(t, f, true, true, true)
	// Source/config replacement without reacquisition stays unavailable; fresh
	// missing child coverage must not inherit the available root forwarder.
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, name := range []string{"secondary", "stub"} {
			var desired public.DNSZone
			if err := tx.Get(ctx, store.Key{Kind: "DNSZone", Name: name}, &desired); err != nil {
				return err
			}
			if name == "secondary" {
				desired.Spec.Secondary.Primaries = []string{"127.0.0.1:1"}
			} else {
				desired.Spec.Stub.Nameservers = []string{"tcp://127.0.0.1:1"}
			}
			if _, err := tx.Apply(ctx, &desired, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		for _, o := range []store.Object{
			&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "pending"}, Spec: public.DNSZoneSpec{ZoneName: "pending.test.", Type: public.ZoneTypeSecondary, Secondary: &public.SecondarySpec{Primaries: []string{"127.0.0.1:1"}}}},
			&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "new-diagnosis", Namespace: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"missing"}}},
		} {
			if _, err := tx.Apply(ctx, o, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	})
	infoWaitTransferred(t, f, false, false, true)
	fresh = infoReadMessages(t, objectReadGET(t.Context(), server, f.token, "info/messages"))
	for _, want := range [][2]string{{"secondary", "DNSZone/secondary"}, {"stub", "DNSZone/stub"}, {"secondary", "DNSZone/pending"}, {"policy", "Policy/default/new-diagnosis"}} {
		if !infoReadHas(fresh, want[0], want[1]) {
			t.Fatal("fresh unavailable coverage or diagnosis omitted", want, fresh)
		}
	}
	close(h.resume)
	held := infoReadMessages(t, <-done)
	if infoReadHas(held, "secondary", "DNSZone/secondary") || infoReadHas(held, "stub", "DNSZone/stub") || infoReadHas(held, "secondary", "DNSZone/pending") || infoReadHas(held, "policy", "Policy/default/new-diagnosis") {
		t.Fatal("held HTTP A leaked B claims", held)
	}
	if err := h.InfoMessageReads.InfoMessages(original, f.time(), func(string, string, string, string, string, string) error { return nil }); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("completed original lease remained usable", err)
	}
}

func TestInfoMessageReadsActualSQLitePolicyLeaseAndCallbackFailure(t *testing.T) {
	f := newFixture(t)
	if _, err := f.store.Apply(t.Context(), &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "diagnostic", Namespace: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"old-missing"}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	h := &heldInfoReads{InfoMessageReads: NewInfoMessageReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	server := infoReadHTTP(t, f, f.pub, h, func() time.Time { return f.now })
	original, done := holdInfoRead(t, h, server, f.token)
	old := infoReadOriginal(original, t, h.InfoMessageReads, f.now)
	if !infoReadHas(old, "policy", "Policy/default/diagnostic") {
		t.Fatal("compiled policy diagnosis missing", old)
	}
	abort := errors.New("stop emission")
	calls := 0
	if err := h.InfoMessageReads.InfoMessages(original, f.now, func(string, string, string, string, string, string) error { calls++; return abort }); err != abort || calls != 1 {
		t.Fatal("callback error swallowed or emission continued", err, calls)
	}
	ctx, cancel := context.WithCancel(original)
	cancel()
	if err := h.InfoMessageReads.InfoMessages(ctx, f.now, func(string, string, string, string, string, string) error {
		t.Fatal("canceled request emitted")
		return nil
	}); err == nil {
		t.Fatal("canceled original request accepted")
	}
	ctx, cancel = context.WithCancel(original)
	if err := h.InfoMessageReads.InfoMessages(ctx, f.now, func(string, string, string, string, string, string) error {
		cancel()
		return nil
	}); err == nil {
		t.Fatal("successful last emission hid cancellation before completion")
	}
	if err := h.InfoMessageReads.InfoMessages(original, f.now, nil); err == nil {
		t.Fatal("nil callback accepted")
	}
	if err := h.InfoMessageReads.InfoMessages(original, time.Time{}, func(string, string, string, string, string, string) error { return nil }); err == nil {
		t.Fatal("zero observation accepted")
	}
	var p public.Policy
	if err := f.store.Get(t.Context(), store.Key{Kind: "Policy", Namespace: "default", Name: "diagnostic"}, &p); err != nil {
		t.Fatal(err)
	}
	p.Spec.ClientGroupRefs = nil
	if _, err := f.store.Apply(t.Context(), &p, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	close(h.resume)
	if got := infoReadMessages(t, <-done); !infoReadHas(got, "policy", "Policy/default/diagnostic") {
		t.Fatal("held policy diagnosis crossed publication", got)
	}
	fresh := infoReadMessages(t, objectReadGET(t.Context(), server, f.token, "info/messages"))
	if infoReadHas(fresh, "policy", "Policy/default/diagnostic") {
		t.Fatal("fresh policy retained old diagnosis", fresh)
	}
	for _, r := range []*InfoMessageReads{nil, NewInfoMessageReads()} {
		if err := r.InfoMessages(context.Background(), f.now, func(string, string, string, string, string, string) error { return nil }); err == nil {
			t.Fatal("missing lease accepted")
		}
	}
}

func TestInfoMessageReadsActualSQLiteRejectsMixedOwners(t *testing.T) {
	f := newFixture(t)
	for _, change := range []string{"objects-generation", "policy-generation", "missing-lists", "wrong-zones", "missing-primary"} {
		t.Run(change, func(t *testing.T) {
			p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
				prepared, err := f.builder.Prepare(ctx, c)
				if err != nil {
					return prepared, err
				}
				for i := range prepared.Components {
					part := &prepared.Components[i]
					switch part.Name {
					case "objects":
						if change == "objects-generation" {
							cloned := *part.Value.(*zonecache.Snapshot)
							cloned.Generation++
							part.Value = &cloned
						}
						if change == "missing-lists" {
							old := part.Value.(*zonecache.Snapshot)
							var raw []store.Object
							for _, kind := range old.Objects.Kinds() {
								raw = append(raw, old.Objects.List(kind)...)
							}
							part.Value, err = zonecache.Prepare(zonecache.PrepareOptions{Kinds: zonecache.DefaultKinds(), Generation: old.Generation}, raw, nil)
							if err != nil {
								return prepared, err
							}
						}
					case "policy":
						if change == "policy-generation" {
							part.Value = policy.Build(policy.Input{Generation: c.Image.Revision + 1}, policy.Options{})
						}
					case "zones":
						if change == "wrong-zones" {
							part.Value = &servingzones.Prepared{}
						}
					case "runtime":
						if change == "missing-primary" {
							cloned := *part.Value.(*RuntimeOwner)
							cloned.primary = nil
							part.Value = &cloned
						}
					}
				}
				return prepared, nil
			})
			server := infoReadHTTP(t, f, p, NewInfoMessageReads(), func() time.Time { return f.now })
			diagnosticWant(t, objectReadGET(t.Context(), server, f.token, "info/messages").diagnosticResponse, http.StatusServiceUnavailable)
		})
	}
}

func TestInfoMessageReadsActualSQLiteRecreatedUIDAndDisabledFeed(t *testing.T) {
	f := newInfoTransferFixture(t)
	h := &heldInfoReads{InfoMessageReads: NewInfoMessageReads(), entered: make(chan context.Context, 1), resume: make(chan struct{})}
	server := infoReadHTTP(t, f.fixture, f.publisher, h, f.time)
	original, done := holdInfoRead(t, h, server, f.token)
	baseline := infoReadOriginal(original, t, h.InfoMessageReads, f.time())
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, name := range []string{"secondary", "stub"} {
			var old public.DNSZone
			key := store.Key{Kind: "DNSZone", Name: name}
			if err := tx.Get(ctx, key, &old); err != nil {
				return err
			}
			if err := tx.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
			created, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: old.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				return err
			}
			if created.GetUID() == old.UID {
				return errors.New("fixture did not recreate actual UID")
			}
		}
		var feed public.BlockList
		if err := tx.Get(ctx, store.Key{Kind: "BlockList", Namespace: "default", Name: "rpz"}, &feed); err != nil {
			return err
		}
		feed.Annotations = map[string]string{public.EnabledAnnotation: "false"}
		if _, err := tx.Apply(ctx, &feed, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		_, err := tx.Apply(ctx, &public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "disabled-ref", Namespace: "default"}, Spec: public.PolicySpec{BlockListRefs: []string{"rpz"}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	infoWaitTransferred(t, f, false, false, true) // Disabled feeds are omitted by selection.
	fresh := infoReadMessages(t, objectReadGET(t.Context(), server, f.token, "info/messages"))
	if !infoReadHas(fresh, "secondary", "DNSZone/secondary") || !infoReadHas(fresh, "stub", "DNSZone/stub") || infoReadHas(fresh, "blocklists", "BlockList/default/rpz") {
		t.Fatal("recreated UID adopted old acquisition or disabled list became load failure", fresh)
	}
	if !slices.ContainsFunc(fresh, func(m apihttp.DiagnosisMessage) bool {
		return m.Source == "policy" && m.Object == "Policy/default/disabled-ref" && m.Reason == policy.ReasonDisabled && m.Severity == "info"
	}) {
		t.Fatal("disabled reference did not retain compiled info severity", fresh)
	}
	if got := infoReadOriginal(original, t, h.InfoMessageReads, f.time()); !slices.Equal(got, baseline) {
		t.Fatal("UID replacement changed held owner", got, baseline)
	}
	close(h.resume)
	infoReadMessages(t, <-done)
}

func TestInfoMessageReadsActualSQLiteOriginalSnapshotConditions(t *testing.T) {
	f := newFixture(t)
	p := catalogReadStartPublisher(t, f.store, func(ctx context.Context, c serving.Candidate) (serving.Prepared, error) {
		prepared, err := f.builder.Prepare(ctx, c)
		if err != nil {
			return prepared, err
		}
		for i := range prepared.Components {
			if prepared.Components[i].Name == "objects" {
				cloned := *prepared.Components[i].Value.(*zonecache.Snapshot)
				// Explicit immutable-owner fixture. Production semantic objects
				// are currently Live, so no live fallback can supply this state.
				cloned.State, cloned.Stale = zonecache.FromSnapshot, true
				prepared.Components[i].Value = &cloned
			}
		}
		return prepared, nil
	})
	server := infoReadHTTP(t, f, p, NewInfoMessageReads(), func() time.Time { return f.now })
	got := infoReadMessages(t, objectReadGET(t.Context(), server, f.token, "info/messages"))
	if !slices.ContainsFunc(got, func(m apihttp.DiagnosisMessage) bool {
		return m.Source == "zonecache" && m.Reason == "ServingFromSnapshot" && m.Severity == "warning"
	}) {
		t.Fatal("original snapshot condition omitted", got)
	}
}
