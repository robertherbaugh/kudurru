package servingbuild

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	apihttp "github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/currentguard"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/driver"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zone"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Explicit test-only issuer. A real native listener must first verify the MAC;
// this helper cannot turn key names or fabricated pipeline metadata into proof.
type diagnosticIssuer struct {
	f     *fixture
	id    zk.ZoneID
	fence pr.PublicationFence
	retry atomic.Uint64
}
type diagnosticIssued struct {
	issuer   *diagnosticIssuer
	req      pipeline.Request
	wire     []byte
	identity pr.ProofIdentity
}
type diagnosticLeaseKey struct{}

func (p *diagnosticIssuer) capture(req *pipeline.Request, apex string) (pr.VerifiedUpdate, error) {
	lease, ok := req.Ctx.Value(diagnosticLeaseKey{}).(*serving.Lease)
	if !ok || apex != p.id.Apex {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	if err := p.f.pub.CheckWriterLease(req.Ctx, lease, serving.TermActivation{Term: p.fence.Term, Incarnation: p.fence.Incarnation}); err != nil {
		return pr.VerifiedUpdate{}, err
	}
	value, _ := lease.Bundle().Component("runtime")
	owner, ok := value.(*RuntimeOwner)
	if !ok || !owner.Primary().Admitted(apex, time.Now()) {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	m, ok := owner.Primary().Cut(time.Now()).Member(apex)
	if !ok || m.Zone != p.id {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	proof, ok := dnsserver.VerifiedTSIGFor(req, time.Now())
	if !ok {
		return pr.VerifiedUpdate{}, pr.ErrUnavailable
	}
	msg := proof.Message()
	msg.Compress = false
	wire, err := msg.Pack()
	if err != nil {
		return pr.VerifiedUpdate{}, err
	}
	digest, err := pr.SemanticDigest(wire)
	if err != nil {
		return pr.VerifiedUpdate{}, err
	}
	key := proof.Identity()
	captured := *req
	captured.Msg = proof.Message()
	issued := &diagnosticIssued{issuer: p, req: captured, wire: bytes.Clone(wire), identity: pr.ProofIdentity{
		Principal: dynamicupdate.Principal{Key: key.Key, Version: key.CredentialVersion}, ProviderVersion: key.ProviderVersion,
		Algorithm: key.Algorithm, Transport: req.Client.Proto.String(), Domain: "dns-update:" + zk.HeadName(p.id), Subject: key.Key,
		RetryID: strconv.FormatUint(p.retry.Add(1), 10), SemanticDigest: digest, Term: p.fence.Term, AuthorityIncarnation: p.fence.Incarnation, NotAfter: proof.Deadline(),
	}}
	return pr.VerifiedUpdate{Zone: p.id, Wire: wire, Proof: issued}, nil
}
func (p *diagnosticIssuer) Verify(ctx context.Context, opaque any, wire []byte) (pr.ProofIdentity, error) {
	issued, ok := opaque.(*diagnosticIssued)
	if !ok || issued == nil || issued.issuer != p || ctx.Err() != nil || issued.req.Ctx.Err() != nil || !bytes.Equal(wire, issued.wire) {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	if _, ok := dnsserver.VerifiedTSIGFor(&issued.req, time.Now()); !ok {
		return pr.ProofIdentity{}, pr.ErrUnavailable
	}
	return issued.identity, nil
}

func diagnosticNative(t *testing.T, f *fixture, issuer *diagnosticIssuer, r *pr.Runtime) *dnsserver.Registry {
	t.Helper()
	authority := authoritative.New(nil)
	var err error
	authority.Update, err = network.NewUpdate(network.UpdateOptions{Runtime: r, Capture: issuer.capture})
	if err != nil {
		t.Fatal(err)
	}
	chain := pipeline.MustNew(&ingress.Stage{}, authority)
	chain.SetCapture(func(req *pipeline.Request) (pipeline.ServingLease, error) {
		lease, err := f.pub.CaptureHTTP(req.Ctx)
		if err == nil {
			req.Ctx = context.WithValue(pr.WithPublicationFence(req.Ctx, issuer.fence), diagnosticLeaseKey{}, lease)
		}
		return servingdns.Wrap(lease), err
	})
	logger := slog.New(slog.DiscardHandler)
	registry, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, TSIG: f.keyrings.Capture().Keys(), Sockets: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, proto := range []dnsserver.Protocol{dnsserver.ProtoUDP, dnsserver.ProtoTCP} {
		if err := registry.Add(dnsserver.Spec{Protocol: proto, Address: "127.0.0.1:0"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := registry.Bind(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- registry.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if ready, _ := registry.Ready(); ready {
			return registry
		}
		if time.Now().After(deadline) {
			t.Fatal("native listener did not start")
		}
		time.Sleep(time.Millisecond)
	}
}

// Block only the first content read, after middleware has captured/authenticated
// the original HTTP lease. The handler continues using that lease after UPDATE.
type heldDiagnosticReads struct {
	*Diagnostics
	hold    atomic.Bool
	entered chan context.Context
	resume  chan struct{}
}

func (h *heldDiagnosticReads) ReadZone(ctx context.Context, name string) (*zone.Zone, error) {
	if h.hold.CompareAndSwap(true, false) {
		h.entered <- ctx
		select {
		case <-h.resume:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return h.Diagnostics.ReadZone(ctx, name)
}

func diagnosticHTTP(t *testing.T, f *fixture, reads apihttp.ZoneReadSource) *httptest.Server {
	t.Helper()
	cop := http.NewCrossOriginProtection()
	adapter, err := servinghttp.New(servinghttp.Options{Publisher: f.pub, Cookie: session.CookieConfig{Secure: session.SecureAlways}, CrossOriginProtection: cop, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	server, err := apihttp.New(apihttp.Options{Store: f.store, Validator: f.builder.opts.Schema.validator, Config: f.deps.Host, Authenticate: adapter.Middleware, Authorizer: adapter, ZoneReads: reads, CrossOrigin: cop, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	t.Cleanup(httpServer.Close)
	return httpServer
}

type diagnosticResponse struct {
	code int
	body string
	err  error
}

func diagnosticGET(ctx context.Context, server *httptest.Server, token, path string) diagnosticResponse {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/api/v1/zones/"+path, nil)
	if err != nil {
		return diagnosticResponse{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	response, err := server.Client().Do(req)
	if err != nil {
		return diagnosticResponse{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	body, err := io.ReadAll(response.Body)
	return diagnosticResponse{code: response.StatusCode, body: string(body), err: err}
}
func diagnosticWant(t *testing.T, response diagnosticResponse, code int) string {
	t.Helper()
	if response.err != nil || response.code != code {
		t.Fatalf("HTTP %d want %d: %s (%v)", response.code, code, response.body, response.err)
	}
	return response.body
}
func diagnosticFind(t *testing.T, cut *DiagnosticCut, name string) ZoneDiagnostic {
	t.Helper()
	zones, err := cut.Zones()
	if err != nil {
		t.Fatal(err)
	}
	for _, z := range zones {
		if z.ObjectName == name {
			return z
		}
	}
	t.Fatal("missing diagnostic", name, zones)
	return ZoneDiagnostic{}
}

func TestDiagnosticsActualNativeUpdateOriginalHTTPAndInternalCuts(t *testing.T) {
	ring, err := dnsserver.NewStaticKeyring(dnsserver.TSIGKey{Name: "key.", Algorithm: dns.HmacSHA256, Secret: []byte("diagnostics-test-secret")})
	if err != nil {
		t.Fatal(err)
	}
	keyrings, err := NewKeyringOwner(ring)
	if err != nil {
		t.Fatal(err)
	}
	f := newFixtureWithKeyrings(t, keyrings)
	guard, err := currentguard.New(currentguard.Options{Pin: keyrings.PrimaryPin(f.pub)})
	if err != nil {
		t.Fatal(err)
	}
	a, err := sqliteadapter.New(sqliteadapter.Options{Store: f.store, Background: guard.Background(), Credentials: guard.Credentials()})
	if err != nil {
		t.Fatal(err)
	}
	backend, err := f.store.ZoneStateBackend(zonestate.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	publication, err := NewPrimaryPublication(f.pub, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	authority := f.store.Authority()
	issuer := &diagnosticIssuer{f: f, fence: pr.PublicationFence{Term: authority.Term, Incarnation: authority.Incarnation}}
	r, err := pr.New(pr.Options{Store: f.store, Repository: zonestate.NewRepository(backend, zonestate.Limits{}), Reader: primaryzone.NewReader(f.store, primaryzone.ReaderOptions{Policy: guard.CapturePolicy}), LifecycleCAS: a.LifecycleCAS, Admit: a.Admission, Publish: publication.Publish, Proofs: issuer})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(pr.WithPublicationFence(t.Context(), issuer.fence), 20*time.Second)
	defer cancel()
	desired := &public.DNSZone{}
	if err := f.store.Get(ctx, store.Key{Kind: "DNSZone", Name: "uncommitted"}, desired); err != nil {
		t.Fatal(err)
	}
	desired.Spec.Policy.QueryAccess, desired.Spec.Policy.Update = public.AccessAllow, public.AccessAllow
	desired.Spec.Policy.UpdatePolicies = []public.UpdatePolicy{{TSIGKeyRef: "key.", Name: "*.uncommitted.example."}}
	if _, err := f.store.Apply(ctx, desired, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	desired, lifecycle := beginPrimary(ctx, t, f, r)
	issuer.id = lifecycle.Spec.Zone
	if _, err := r.Reconcile(ctx, issuer.id); err != nil {
		t.Fatal(err)
	}
	diagnostics, err := NewDiagnostics(f.pub, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	reads := &heldDiagnosticReads{Diagnostics: diagnostics, entered: make(chan context.Context, 1), resume: make(chan struct{})}
	server := diagnosticHTTP(t, f, reads)
	registry := diagnosticNative(t, f, issuer, r)
	before, err := diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer before.Release()
	old := diagnosticFind(t, before, desired.Name)
	if !old.Available || old.HeadVersion.CommitSequence == 0 || old.Names == 0 {
		t.Fatal("initial committed metrics absent", old)
	}
	zones, _ := before.Zones()
	zones[0].Apex = "mutated"
	if again := diagnosticFind(t, before, desired.Name); again != old {
		t.Fatal("diagnostics returned mutable state", again, old)
	}
	initial := diagnosticWant(t, diagnosticGET(ctx, server, f.token, desired.Name+"/export"), http.StatusOK)
	reads.hold.Store(true)
	heldResponse := make(chan diagnosticResponse, 1)
	go func() { heldResponse <- diagnosticGET(ctx, server, f.token, desired.Name+"/export") }()
	var heldCtx context.Context
	select {
	case heldCtx = <-reads.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for i, proto := range []dnsserver.Protocol{dnsserver.ProtoUDP, dnsserver.ProtoTCP} {
		msg := new(dns.Msg)
		msg.SetUpdate(desired.Spec.ZoneName)
		rr, err := dns.NewRR(fmt.Sprintf("dynamic%d.%s 60 IN A 192.0.2.%d", i, desired.Spec.ZoneName, i+1))
		if err != nil {
			t.Fatal(err)
		}
		msg.Insert([]dns.RR{rr})
		msg.SetTsig("key.", dns.HmacSHA256, 300, time.Now().Unix())
		client := &dns.Client{Net: string(proto), Timeout: 5 * time.Second, TsigProvider: dnsserver.TSIGProvider(ring)}
		response, _, err := client.ExchangeContext(ctx, msg, registry.Addr(proto).String())
		if err != nil || response == nil || response.Rcode != dns.RcodeSuccess || response.IsTsig() == nil {
			t.Fatalf("native %s update failed: %v %v", proto, response, err)
		}
	}
	close(reads.resume)
	held := diagnosticWant(t, <-heldResponse, http.StatusOK)
	if held != initial || strings.Contains(held, "dynamic0") {
		t.Fatal("original HTTP lease changed across UPDATE", held)
	}
	if _, err := diagnostics.ReadZone(heldCtx, desired.Name); err == nil {
		t.Fatal("released HTTP request recaptured a model")
	}
	if _, err := diagnostics.ReadZone(ctx, desired.Name); err == nil {
		t.Fatal("public read accepted missing original lease")
	}
	after, err := diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer after.Release()
	current := diagnosticFind(t, after, desired.Name)
	if !current.Available || current.HeadVersion.CommitSequence <= old.HeadVersion.CommitSequence || current.Serial == old.Serial || current.RecordCount != old.RecordCount+2 || current.Names != old.Names+2 || current.ContentHash == old.ContentHash {
		t.Fatal("actual committed metrics did not advance", old, current)
	}
	if heldOld := diagnosticFind(t, before, desired.Name); heldOld != old {
		t.Fatal("held internal cut mixed heads", heldOld, old)
	}
	var records apihttp.List[apihttp.ZoneRecord]
	body := diagnosticWant(t, diagnosticGET(ctx, server, f.token, desired.Name+"/records"), http.StatusOK)
	if err := json.Unmarshal([]byte(body), &records); err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, row := range records.Items {
		if strings.HasPrefix(row.DNSName, "dynamic") {
			found++
			if row.RecordSet != "" || row.ManagedBy != "" {
				t.Fatal("dynamic RR fabricated desired ownership", row)
			}
		}
	}
	if found != 2 {
		t.Fatal("fresh records missed native UPDATE", body)
	}
	exported := diagnosticWant(t, diagnosticGET(ctx, server, f.token, desired.Name+"/export"), http.StatusOK)
	if !strings.Contains(exported, "dynamic0.") || !strings.Contains(exported, "dynamic1.") || !strings.Contains(exported, fmt.Sprintf("serial %d", current.Serial)) {
		t.Fatal("fresh export recompiled desired data", exported)
	}
	var summary apihttp.ZoneDetail
	if err := json.Unmarshal([]byte(diagnosticWant(t, diagnosticGET(ctx, server, f.token, desired.Name), http.StatusOK)), &summary); err != nil {
		t.Fatal(err)
	}
	if !summary.Ready || summary.Serial != current.Serial || summary.RecordCount != current.RecordCount || summary.RecordSets != 0 {
		t.Fatal("summary did not use actual held head", summary, current)
	}
	if err := f.store.Get(ctx, store.KeyOf(desired), desired); err != nil {
		t.Fatal(err)
	}
	if desired.Status.Serial != 0 {
		t.Fatal("test depended on controller status restamping", desired.Status)
	}
	before.Release()
	before.Release()
	if _, err := before.Zones(); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("released diagnostics remained readable", err)
	}
	canceled, stop := context.WithCancel(ctx)
	cut, err := diagnostics.Capture(canceled)
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Release()
	stop()
	if _, err := cut.Zones(); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled internal cut remained readable", err)
	}
}

func TestDiagnosticsActualDesiredGapReplacementAndEligibility(t *testing.T) {
	f := newFixture(t)
	diagnostics, err := NewDiagnostics(f.pub, func() time.Time { return f.now })
	if err != nil {
		t.Fatal(err)
	}
	server := diagnosticHTTP(t, f, diagnostics)
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, "uncommitted/records"), http.StatusServiceUnavailable)
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, "missing/records"), http.StatusNotFound)
	r, _, writerCtx := primaryFixture(t, f)
	z, l := beginPrimary(writerCtx, t, f, r)
	if _, err := r.Reconcile(writerCtx, l.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	old, err := diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Release()
	before := diagnosticFind(t, old, z.Name)
	if !before.Available {
		t.Fatal("primary not activated", before)
	}
	if err := f.store.Get(ctx, store.KeyOf(z), z); err != nil {
		t.Fatal(err)
	}
	z.Spec.Nameservers = []string{"changed." + z.Spec.ZoneName}
	if _, err := f.store.Apply(ctx, z, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	f.waitCurrent(t)
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, z.Name+"/export"), http.StatusServiceUnavailable)
	fresh, err := diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	gap := diagnosticFind(t, fresh, z.Name)
	fresh.Release()
	if gap.Available || gap.Names != 0 || gap.HeadVersion != before.HeadVersion || gap.ContentHash != before.ContentHash {
		t.Fatal("source gap lost evidence or served old config", gap, before)
	}
	if again := diagnosticFind(t, old, z.Name); again != before {
		t.Fatal("held candidate mutated", again, before)
	}
	if err := f.store.Delete(ctx, store.KeyOf(z), store.DeleteOptions{Manager: store.ManagerSystem}); err != nil {
		t.Fatal(err)
	}
	object, err := f.store.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: z.Name}, Spec: z.Spec}, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	if object.GetUID() == z.UID {
		t.Fatal("replacement reused UID")
	}
	f.waitCurrent(t)
	fresh, err = diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	replaced := diagnosticFind(t, fresh, z.Name)
	fresh.Release()
	if replaced.Available || replaced.Names != 0 || replaced.Serial != 0 || replaced.RecordCount != 0 || replaced.HeadVersion.CommitSequence != 0 || replaced.UID == before.UID {
		t.Fatal("replacement borrowed old UID head", replaced, before)
	}
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, z.Name+"/records"), http.StatusServiceUnavailable)
	f.blocked.Store(true)
	f.epoch.Add(1)
	f.pub.Wake()
	if cut, err := diagnostics.Capture(ctx); err == nil {
		cut.Release()
		t.Fatal("ineligible complete cut captured")
	}
	if _, err := NewDiagnostics(nil, nil); !errors.Is(err, serving.ErrComponents) {
		t.Fatal("nil publisher accepted", err)
	}
	var absent *Diagnostics
	if _, err := absent.Capture(ctx); err == nil {
		t.Fatal("nil diagnostics accepted")
	}
	var cut *DiagnosticCut
	if _, err := cut.Zones(); !errors.Is(err, serving.ErrReleased) {
		t.Fatal("nil cut readable", err)
	}
	cut.Release()
}

func TestDiagnosticsActualAncestorDeadlineAndHeldEvidence(t *testing.T) {
	f := newFixture(t)
	r, publication, ctx := primaryFixture(t, f)
	_, childLifecycle := beginPrimary(ctx, t, f, r)
	if _, err := r.Reconcile(ctx, childLifecycle.Spec.Zone); err != nil {
		t.Fatal(err)
	}
	expiry := f.now.Add(time.Hour).Truncate(time.Second)
	f.command(t, func(ctx context.Context, tx *sqlite.CommandTx) error {
		parent, err := tx.Apply(ctx, &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "parent"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "lease", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: parent.GetName()}, Endpoints: []public.Endpoint{{DNSName: "lease.example.", RecordType: "A", Targets: []string{"192.0.2.1"}, Kudurru: &public.EndpointExt{ExpiresAt: &metav1.Time{Time: expiry}}}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	runner, err := driver.New(driver.Options{Store: f.store, Runtime: r, Publication: publication, Image: pr.ImageOptions{ClusterID: f.store.Info().ClusterID, Scheme: f.builder.opts.Schema.scheme, Validation: f.builder.opts.Schema.validation(ctx)}, PollInterval: 10 * time.Millisecond, Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	writer, _ := pr.PublicationFenceFrom(ctx)
	runctx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- runner.Run(runctx, writer) }()
	defer func() {
		cancel()
		if err := <-done; err != nil && !errors.Is(err, context.Canceled) {
			t.Error(err)
		}
	}()
	deadline := time.After(15 * time.Second)
	for {
		changed := f.pub.Subscribe()
		lease, err := f.pub.CaptureHTTP(ctx)
		if err == nil {
			value, _ := lease.Bundle().Component("runtime")
			primary := value.(*RuntimeOwner).Primary()
			ready := primary.Admitted("example.", f.now) && primary.Admitted("uncommitted.example.", f.now)
			lease.Release()
			if ready {
				break
			}
		}
		select {
		case <-changed:
		case <-deadline:
			t.Fatalf("late parent did not activate: %+v publisher=%+v", runner.Status(), f.pub.Status())
		}
	}
	var clock atomic.Int64
	clock.Store(expiry.Add(-time.Nanosecond).UnixNano())
	diagnostics, err := NewDiagnostics(f.pub, func() time.Time { return time.Unix(0, clock.Load()) })
	if err != nil {
		t.Fatal(err)
	}
	cut, err := diagnostics.Capture(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer cut.Release()
	before := diagnosticFind(t, cut, "uncommitted")
	if !before.Available || before.Names == 0 || before.HeadVersion.CommitSequence == 0 {
		t.Fatal("child not available before parent deadline", before)
	}
	server := diagnosticHTTP(t, f, diagnostics)
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, "uncommitted/export"), http.StatusOK)
	clock.Store(expiry.UnixNano())
	after := diagnosticFind(t, cut, "uncommitted")
	if after.Available || after.Names != 0 || after.Serial != before.Serial || after.ContentHash != before.ContentHash || after.HeadVersion != before.HeadVersion {
		t.Fatal("parent deadline ignored or rewrote durable evidence", before, after)
	}
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, "uncommitted/records"), http.StatusServiceUnavailable)
	diagnosticWant(t, diagnosticGET(ctx, server, f.token, "uncommitted/export"), http.StatusServiceUnavailable)
	var detail apihttp.ZoneDetail
	body := diagnosticWant(t, diagnosticGET(ctx, server, f.token, "uncommitted"), http.StatusOK)
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Ready || detail.Serial != before.Serial || detail.RecordCount != before.RecordCount {
		t.Fatal("expired child summary invented readiness or lost head", detail)
	}
}
