package webhook

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/externaldns"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
	"github.com/robertherbaugh/kudurru/internal/credentials"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// recordsProvider provisions and checks a publishing instance against a
// TLS peer served at the root of server.
func recordsProvider(t *testing.T, server *httptest.Server, domains []string, policy string) *provider {
	t.Helper()
	values := map[string]string{"bearerToken": "explicit-test-token", "ca": string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))}
	p := newProvider()
	t.Cleanup(func() { _ = p.Stop(context.Background()) })
	raw, err := json.Marshal(public.WebhookIntegration{Endpoint: server.URL, Domains: domains, Policy: policy})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Provision(t.Context(), &lifecycleHost{}, raw, namedResolver(func(_ context.Context, name string) (string, error) {
		if v, ok := values[name]; ok {
			return v, nil
		}
		return "", credentials.ErrDenied
	})); err != nil {
		t.Fatal("provision", err)
	}
	return p
}

func checked(t *testing.T, p *provider) {
	t.Helper()
	if _, err := p.Check(t.Context()); err != nil {
		t.Fatal("check", err)
	}
	if roles, err := p.Roles(); err != nil || !roles.Publisher {
		t.Fatal("records peer with domains is no publisher", roles, err)
	}
}

// recordsPlan builds the host's plan for desired against a fresh read.
func recordsPlan(t *testing.T, p *provider, desired []public.Endpoint) (*plan.Plan, host.RemoteSnapshot) {
	t.Helper()
	pubs, err := p.Destinations(t.Context())
	if err != nil || len(pubs) != 1 {
		t.Fatal("destinations", pubs, err)
	}
	adjusted, err := p.Adjust(t.Context(), pubs[0], desired)
	if err != nil {
		t.Fatal("adjust", err)
	}
	snapshot, err := p.Records(t.Context(), pubs[0].Destination)
	if err != nil || !snapshot.Complete {
		t.Fatal("records", err)
	}
	built, err := plan.Build(plan.Input{Owner: p.owner, Destination: pubs[0].Destination, ConfigDigest: strings.Repeat("c", 64), CredentialDigest: strings.Repeat("d", 64), SourceDigest: strings.Repeat("e", 64), Policy: pubs[0].Policy, Desired: adjusted, Current: snapshot.Records, DesiredComplete: true, CurrentComplete: true})
	if err != nil {
		t.Fatal("plan", err)
	}
	return built, snapshot
}

func endpointA(name string, targets ...string) public.Endpoint {
	return public.Endpoint{DNSName: name, RecordType: "A", RecordTTL: 300, Targets: targets}
}

func firstPartyPeer(t *testing.T) (*httptest.Server, *externaldns.Server, *atomic.Int32) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	zone := &public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "example"}, Spec: public.DNSZoneSpec{ZoneName: "example.test.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example.test."}}}
	if _, err := st.Apply(t.Context(), zone, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("explicit-test-token"), 0600); err != nil {
		t.Fatal(err)
	}
	ed, err := externaldns.New(externaldns.Options{Store: st, Validator: v, Namespace: "external-dns", TokenFile: token, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	var posts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/records" {
			posts.Add(1)
		}
		ed.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	return server, ed, &posts
}

func peerRows(t *testing.T, ed *externaldns.Server) map[string][]string {
	t.Helper()
	eps, err := ed.Records(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]string{}
	for _, e := range eps {
		out[e.DNSName+"/"+e.RecordType] = slices.Clone(e.Targets)
	}
	return out
}

// The first-party external-dns listener is an independent peer: the
// instance creates, updates and deletes a record set with its registry row
// there, every receipt is confirmed from a reread, and an applied intent is
// never sent twice.
func TestRecordsFirstPartyExternalDNSLifecycle(t *testing.T) {
	server, ed, posts := firstPartyPeer(t)
	p := recordsProvider(t, server, []string{"example.test"}, "")
	checked(t, p)
	marker := registryValue(registryMarker(p.owner))

	built, _ := recordsPlan(t, p, []public.Endpoint{endpointA("www.example.test.", "192.0.2.1")})
	if len(built.Operations) != 1 || built.Operations[0].Action != "create" {
		t.Fatal("create not planned", built.Operations)
	}
	create := built.Intent(0, time.Now())
	receipt, err := p.Apply(t.Context(), create)
	if err != nil || !receipt.Complete || receipt.ContentHash != create.Spec.AfterHash || len(receipt.ProviderIDs) != 1 || posts.Load() != 1 {
		t.Fatal("create receipt", receipt, err)
	}
	rows := peerRows(t, ed)
	if !slices.Equal(rows["www.example.test/A"], []string{"192.0.2.1"}) || !slices.Equal(rows["_kudurru-a.www.example.test/TXT"], []string{marker}) || len(rows) != 2 {
		t.Fatal("peer rows after create", rows)
	}
	// A complete intent is recognized from the reread, not sent again.
	if again, err := p.Apply(t.Context(), create); err != nil || !again.Complete || posts.Load() != 1 {
		t.Fatal("applied intent was replayed", err)
	}

	built, snapshot := recordsPlan(t, p, []public.Endpoint{endpointA("www.example.test.", "192.0.2.2")})
	if len(snapshot.Records) != 1 || snapshot.Records[0].Owner != (plan.RemoteOwner{ClusterID: "fixture", IntegrationUID: "original"}) {
		t.Fatal("registry ownership not observed or registry row shown", snapshot.Records)
	}
	if len(built.Operations) != 1 || built.Operations[0].Action != "update" {
		t.Fatal("update not planned", built.Operations)
	}
	receipt, err = p.Apply(t.Context(), built.Intent(0, time.Now()))
	if err != nil || !receipt.Complete || posts.Load() != 2 {
		t.Fatal("update receipt", receipt, err)
	}
	if rows := peerRows(t, ed); !slices.Equal(rows["www.example.test/A"], []string{"192.0.2.2"}) || len(rows) != 2 {
		t.Fatal("peer rows after update", rows)
	}

	built, _ = recordsPlan(t, p, nil)
	if len(built.Operations) != 1 || built.Operations[0].Action != "delete" {
		t.Fatal("delete not planned", built.Operations)
	}
	receipt, err = p.Apply(t.Context(), built.Intent(0, time.Now()))
	if err != nil || !receipt.Complete || posts.Load() != 3 {
		t.Fatal("delete receipt", receipt, err)
	}
	// The ledger confirms a receipt only when the stored operation equals the
	// one it saved; a receipt must survive the store's JSON round trip as is.
	if b, err := json.Marshal(receipt); err != nil {
		t.Fatal(err)
	} else {
		var stored kinds.Receipt
		if err := json.Unmarshal(b, &stored); err != nil || !reflect.DeepEqual(stored, receipt) {
			t.Fatalf("delete receipt changes in the store: %#v != %#v", stored, receipt)
		}
	}
	if rows := peerRows(t, ed); len(rows) != 0 {
		t.Fatal("data or registry row left after delete", rows)
	}
}

// scriptedPeer is an external-dns peer whose behavior a test chooses.
type scriptedPeer struct {
	mu       sync.Mutex
	rows     []*externaldns.Endpoint
	posts    int
	mode     string
	pageNext bool
	failRead atomic.Bool
	listed   func() []*externaldns.Endpoint
}

func (s *scriptedPeer) serve(t *testing.T) *httptest.Server {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer explicit-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", externalMedia)
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/":
			_, _ = io.WriteString(w, `{"include":["example.test"],"exclude":["private.example.test"]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/records":
			if s.failRead.Load() {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			if s.pageNext {
				w.Header().Set("Link", `</records?page=2>; rel="next"`)
			}
			rows := s.rows
			if s.listed != nil {
				rows = s.listed()
			}
			if rows == nil {
				rows = []*externaldns.Endpoint{}
			}
			_ = json.NewEncoder(w).Encode(rows)
		case r.Method == http.MethodPost && r.URL.Path == "/adjustendpoints":
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write(b)
		case r.Method == http.MethodPost && r.URL.Path == "/records":
			s.posts++
			var ch externaldns.Changes
			if json.NewDecoder(r.Body).Decode(&ch) != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			switch s.mode {
			case "no-effect":
			case "foreign-change":
				for _, e := range s.rows {
					if e.RecordType == "A" {
						e.Targets = []string{"198.51.100.9"}
					}
				}
			case "applied-unreadable":
				s.apply(&ch)
				s.failRead.Store(true)
			case "drop-registry", "unavailable", "unavailable-applied", "":
				if s.mode == "unavailable" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				s.apply(&ch)
				if s.mode == "unavailable-applied" {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(server.Close)
	return server
}

func (s *scriptedPeer) apply(ch *externaldns.Changes) {
	same := func(a, b *externaldns.Endpoint) bool {
		return strings.TrimSuffix(a.DNSName, ".") == strings.TrimSuffix(b.DNSName, ".") && a.RecordType == b.RecordType && a.SetIdentifier == b.SetIdentifier
	}
	remove := func(e *externaldns.Endpoint) {
		s.rows = slices.DeleteFunc(s.rows, func(r *externaldns.Endpoint) bool { return same(r, e) })
	}
	for _, e := range append(slices.Clone(ch.Delete), ch.UpdateOld...) {
		remove(e)
	}
	for _, e := range append(slices.Clone(ch.Create), ch.UpdateNew...) {
		if s.mode == "drop-registry" && strings.HasPrefix(e.DNSName, registryPrefix) {
			continue
		}
		remove(e)
		s.rows = append(s.rows, e)
	}
}

func (s *scriptedPeer) postCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.posts
}

func (s *scriptedPeer) set(mode string, rows ...*externaldns.Endpoint) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode, s.rows = mode, rows
}

func wireA(name string, targets ...string) *externaldns.Endpoint {
	return &externaldns.Endpoint{DNSName: name, RecordType: "A", RecordTTL: 300, Targets: targets}
}

func wireTXT(name, value string) *externaldns.Endpoint {
	return &externaldns.Endpoint{DNSName: name, RecordType: "TXT", RecordTTL: 300, Targets: []string{value}}
}

func createIntent(t *testing.T, p *provider) *kinds.IntegrationOperation {
	t.Helper()
	built, _ := recordsPlan(t, p, []public.Endpoint{endpointA("www.example.test.", "192.0.2.1")})
	if len(built.Operations) != 1 || built.Operations[0].Action != "create" {
		t.Fatal("create not planned", built.Operations, built.Conflicts)
	}
	return built.Intent(0, time.Now())
}

// A wire acknowledgment is never a receipt (D298): a peer that accepts the
// body without effect, keeps the data but drops the registry row, or sees a
// foreign change between the reread and the POST yields a partial receipt
// that leaves the ledger pending.
func TestRecordsUnverifiedApplyStaysPartial(t *testing.T) {
	for _, mode := range []string{"no-effect", "drop-registry", "foreign-change"} {
		t.Run(mode, func(t *testing.T) {
			peer := &scriptedPeer{}
			server := peer.serve(t)
			p := recordsProvider(t, server, []string{"example.test"}, "")
			checked(t, p)
			op := createIntent(t, p)
			if mode == "foreign-change" {
				// The foreign row appears only after the planning read.
				peer.mu.Lock()
				peer.listed = func() []*externaldns.Endpoint {
					if peer.posts == 0 {
						return peer.rows
					}
					return []*externaldns.Endpoint{wireA("www.example.test", "198.51.100.9")}
				}
				peer.mu.Unlock()
			}
			peer.set(mode)
			receipt, err := p.Apply(t.Context(), op)
			if !errors.Is(err, errDrift) || receipt.Complete || receipt.ObservedAt.IsZero() || peer.postCount() != 1 {
				t.Fatal("unverified apply was confirmed or replayed", receipt, err)
			}
			for _, fact := range receipt.Targets {
				if fact.Applied {
					t.Fatal("unowned or absent data recorded as applied", receipt.Targets)
				}
			}
			// The body reached the peer, so the attempt is ambiguous: the
			// host keeps the operation pending instead of abandoning it as
			// untouched work and planning it again.
			if len(receipt.Targets) != 1 || receipt.Targets[0].Target != "192.0.2.1" || receipt.Targets[0].ErrorClass != "ambiguous" {
				t.Fatal("an unconfirmed 2xx left no ambiguous target", receipt.Targets)
			}
		})
	}
}

// Narrowing the domains of a publishing instance keeps the record sets its
// registry rows claim visible outside the new domains, so sync deletes them
// with their registry rows instead of the narrowed list making them look
// absent. Unowned rows outside the domains stay hidden and untouched.
func TestRecordsNarrowedDomainsKeepOwnedRowsVisible(t *testing.T) {
	server, ed, posts := firstPartyPeer(t)
	p := recordsProvider(t, server, []string{"a.example.test", "b.example.test"}, "")
	checked(t, p)
	desired := []public.Endpoint{endpointA("www.a.example.test.", "192.0.2.1"), endpointA("www.b.example.test.", "192.0.2.2")}
	built, _ := recordsPlan(t, p, desired)
	for i := range built.Operations {
		if receipt, err := p.Apply(t.Context(), built.Intent(i, time.Now())); err != nil || !receipt.Complete {
			t.Fatal("publish", receipt, err)
		}
	}
	if err := ed.ApplyChanges(t.Context(), &externaldns.Changes{Create: []*externaldns.Endpoint{wireA("manual.b.example.test", "198.51.100.1")}}); err != nil {
		t.Fatal(err)
	}
	narrowed := recordsProvider(t, server, []string{"a.example.test"}, "")
	checked(t, narrowed)
	built, snapshot := recordsPlan(t, narrowed, desired[:1])
	seen := map[string]plan.RemoteOwner{}
	for _, row := range snapshot.Records {
		seen[row.Endpoint.DNSName] = row.Owner
	}
	owner := plan.RemoteOwner{ClusterID: "fixture", IntegrationUID: "original"}
	if len(seen) != 2 || seen["www.a.example.test."] != owner || seen["www.b.example.test."] != owner {
		t.Fatal("narrowed snapshot", seen)
	}
	if len(built.Operations) != 1 || built.Operations[0].Action != "delete" || built.Operations[0].Key.Name != "www.b.example.test." {
		t.Fatal("owned row outside the narrowed domains not planned for deletion", built.Operations)
	}
	before := posts.Load()
	if receipt, err := narrowed.Apply(t.Context(), built.Intent(0, time.Now())); err != nil || !receipt.Complete || posts.Load() != before+1 {
		t.Fatal("delete outside the narrowed domains", receipt, err)
	}
	rows := peerRows(t, ed)
	if rows["www.b.example.test/A"] != nil || rows["_kudurru-a.www.b.example.test/TXT"] != nil || rows["manual.b.example.test/A"] == nil || rows["www.a.example.test/A"] == nil {
		t.Fatal("peer rows after the narrowed sync", rows)
	}
	// A create outside the domains stays refused.
	forged, err := plan.Build(plan.Input{Owner: narrowed.owner, Destination: webhookDestination(server.URL), ConfigDigest: strings.Repeat("c", 64), CredentialDigest: strings.Repeat("d", 64), SourceDigest: strings.Repeat("e", 64), Policy: plan.PolicySync, Desired: []public.Endpoint{endpointA("new.b.example.test.", "192.0.2.3")}, DesiredComplete: true, CurrentComplete: true})
	if err != nil || len(forged.Operations) != 1 {
		t.Fatal(err)
	}
	if _, err := narrowed.Apply(t.Context(), forged.Intent(0, time.Now())); !errors.Is(err, errInput) {
		t.Fatal("create outside the domains accepted", err)
	}
}

// An instance whose domains were all removed still names its destination,
// with no domains, so a cleanup can read and delete the record sets its
// registry rows claim.
func TestRecordsInstanceWithoutDomainsStillCleansUp(t *testing.T) {
	server, ed, _ := firstPartyPeer(t)
	p := recordsProvider(t, server, []string{"example.test"}, "")
	checked(t, p)
	built, _ := recordsPlan(t, p, []public.Endpoint{endpointA("www.example.test.", "192.0.2.1")})
	if receipt, err := p.Apply(t.Context(), built.Intent(0, time.Now())); err != nil || !receipt.Complete {
		t.Fatal("publish", receipt, err)
	}
	if err := ed.ApplyChanges(t.Context(), &externaldns.Changes{Create: []*externaldns.Endpoint{wireA("manual.example.test", "198.51.100.1")}}); err != nil {
		t.Fatal(err)
	}
	// The peer offers only records, so Check refuses a provider built with
	// no domains; this is the provider of an instance that negotiated with
	// domains and lost them, as one whose peer offers more would be.
	p.mu.Lock()
	p.records.domains = nil
	p.mu.Unlock()
	pubs, err := p.Destinations(t.Context())
	if err != nil || len(pubs) != 1 || len(pubs[0].Domains) != 0 || pubs[0].Destination != webhookDestination(server.URL) {
		t.Fatal("destination without domains", pubs, err)
	}
	snapshot, err := p.Records(t.Context(), pubs[0].Destination)
	if err != nil || len(snapshot.Records) != 1 || snapshot.Records[0].Endpoint.DNSName != "www.example.test." {
		t.Fatal("owned rows of an instance without domains", snapshot.Records, err)
	}
	cleanup, err := plan.Build(plan.Input{Owner: p.owner, Destination: pubs[0].Destination, ConfigDigest: strings.Repeat("c", 64), CredentialDigest: strings.Repeat("d", 64), SourceDigest: strings.Repeat("e", 64), Policy: pubs[0].Policy, Current: snapshot.Records, DesiredComplete: true, CurrentComplete: true})
	if err != nil || len(cleanup.Operations) != 1 || cleanup.Operations[0].Action != "delete" {
		t.Fatal("cleanup plan", cleanup, err)
	}
	if receipt, err := p.Apply(t.Context(), cleanup.Intent(0, time.Now())); err != nil || !receipt.Complete {
		t.Fatal("cleanup delete", receipt, err)
	}
	if rows := peerRows(t, ed); len(rows) != 1 || rows["manual.example.test/A"] == nil {
		t.Fatal("peer rows after cleanup", rows)
	}
}

// Unowned, foreign-registry and external-dns-owned rows are protected: the
// planner sees their owners and plans no change, and an intent forged past
// the planner is refused before any POST.
func TestRecordsForeignAndUnownedRowsAreProtected(t *testing.T) {
	foreign := registryValue(strings.Repeat("f", 64))
	for _, tc := range []struct {
		name  string
		rows  []*externaldns.Endpoint
		owner plan.RemoteOwner
	}{
		{"unowned", []*externaldns.Endpoint{wireA("www.example.test", "192.0.2.7")}, plan.RemoteOwner{}},
		{"foreign-registry", []*externaldns.Endpoint{wireA("www.example.test", "192.0.2.7"), wireTXT("_kudurru-a.www.example.test", foreign)}, plan.RemoteOwner{ClusterID: "foreign", IntegrationUID: "kudurru-registry"}},
		{"external-dns-heritage", []*externaldns.Endpoint{wireA("www.example.test", "192.0.2.7"), wireTXT("a-www.example.test", `"heritage=external-dns,external-dns/owner=other"`)}, plan.RemoteOwner{ExternalDNSOwner: "other"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := &scriptedPeer{}
			server := peer.serve(t)
			p := recordsProvider(t, server, []string{"example.test"}, "")
			checked(t, p)
			peer.set("", tc.rows...)
			built, snapshot := recordsPlan(t, p, []public.Endpoint{endpointA("www.example.test.", "192.0.2.1")})
			var owner *plan.RemoteOwner
			for _, row := range snapshot.Records {
				if strings.HasPrefix(row.Endpoint.DNSName, registryPrefix) {
					t.Fatal("registry row shown to the planner")
				}
				if row.Endpoint.DNSName == "www.example.test." && row.Endpoint.RecordType == "A" {
					owner = &row.Owner
				}
			}
			if owner == nil || *owner != tc.owner {
				t.Fatal("owner", owner)
			}
			if len(built.Operations) != 0 || len(built.Conflicts) == 0 {
				t.Fatal("protected row planned", built.Operations)
			}
			// A forged update of the protected row never reaches the peer.
			forged, err := plan.Build(plan.Input{Owner: p.owner, Destination: webhookDestination(server.URL), ConfigDigest: strings.Repeat("c", 64), CredentialDigest: strings.Repeat("d", 64), SourceDigest: strings.Repeat("e", 64), Policy: plan.PolicySync, Desired: []public.Endpoint{endpointA("www.example.test.", "192.0.2.1")}, DesiredComplete: true, CurrentComplete: true})
			if err != nil || len(forged.Operations) != 1 {
				t.Fatal(err)
			}
			if _, err := p.Apply(t.Context(), forged.Intent(0, time.Now())); !errors.Is(err, errDrift) || peer.postCount() != 0 {
				t.Fatal("protected row changed", err)
			}
		})
	}
}

// A refused or unanswered POST is ambiguous: the receipt names its targets
// as ambiguous, keeps the host's retry class, and Recover confirms from a
// reread without sending the body again when the peer did apply it.
func TestRecordsAmbiguousPostAndRecovery(t *testing.T) {
	peer := &scriptedPeer{}
	server := peer.serve(t)
	p := recordsProvider(t, server, []string{"example.test"}, "")
	checked(t, p)
	op := createIntent(t, p)
	peer.set("unavailable-applied")
	receipt, err := p.Apply(t.Context(), op)
	if err != nil || !receipt.Complete || peer.postCount() != 1 {
		t.Fatal("an applied change answered 503 is confirmed only by the reread", receipt, err)
	}
	peer.set("unavailable")
	op = createIntent(t, p)
	receipt, err = p.Apply(t.Context(), op)
	var failure *host.Failure
	if !errors.As(err, &failure) || failure.Kind != host.FailureTemporary || receipt.Complete || len(receipt.Targets) != 1 || receipt.Targets[0].ErrorClass != "ambiguous" {
		t.Fatal("ambiguous post", receipt, err)
	}
	op.Spec.Receipt = &receipt
	peer.set("", wireA("www.example.test", "192.0.2.1"), wireTXT("_kudurru-a.www.example.test", registryValue(registryMarker(p.owner))))
	before := peer.postCount()
	recovered, err := p.Recover(t.Context(), op, host.RemoteSnapshot{Complete: true})
	if err != nil || !recovered.Complete || peer.postCount() != before {
		t.Fatal("recovery did not confirm from the reread", recovered, err)
	}
	// A failed verification reread still leaves a dated partial receipt.
	peer.set("")
	op = createIntent(t, p)
	peer.set("applied-unreadable")
	receipt, err = p.Apply(t.Context(), op)
	if err == nil || receipt.Complete || receipt.ObservedAt.IsZero() {
		t.Fatal("unreadable result confirmed", receipt, err)
	}
}

// Lists the adapter cannot attribute are refused whole: a pagination
// header, a repeated key, and more rows than the bound. A list above the
// generic 4,096-element JSON bound but within the record bound is read.
func TestRecordsListBounds(t *testing.T) {
	peer := &scriptedPeer{}
	server := peer.serve(t)
	p := recordsProvider(t, server, []string{"example.test"}, "")
	checked(t, p)
	destination := webhookDestination(server.URL)
	many := func(n int) []*externaldns.Endpoint {
		out := make([]*externaldns.Endpoint, 0, n)
		for i := range n {
			out = append(out, wireA(fmt.Sprintf("h%d.other.test", i), "192.0.2.1"))
		}
		return out
	}
	peer.set("", many(5000)...)
	if _, err := p.Records(t.Context(), destination); err != nil {
		t.Fatal("list within the record bound refused", err)
	}
	peer.set("", many(maxRecordItems+1)...)
	if _, err := p.Records(t.Context(), destination); !errors.Is(err, errProtocol) {
		t.Fatal("over-bound list accepted", err)
	}
	peer.set("", wireA("www.example.test", "192.0.2.1"), wireA("www.example.test.", "192.0.2.2"))
	if _, err := p.Records(t.Context(), destination); !errors.Is(err, errProtocol) {
		t.Fatal("repeated key accepted", err)
	}
	peer.set("")
	peer.mu.Lock()
	peer.pageNext = true
	peer.mu.Unlock()
	if _, err := p.Records(t.Context(), destination); !errors.Is(err, errProtocol) {
		t.Fatal("paginated list accepted", err)
	}
}

// Configuration: domains need a records peer whose filter covers them,
// excluded names and the registry namespace are never published, and a
// records-only peer without domains has nothing to do.
func TestRecordsNegotiationAndPublishableNames(t *testing.T) {
	peer := &scriptedPeer{}
	server := peer.serve(t)
	p := recordsProvider(t, server, []string{"elsewhere.test"}, "")
	if _, err := p.Check(t.Context()); !errors.Is(err, host.ErrUnsupported) {
		t.Fatal("domain outside the peer filter accepted", err)
	}
	p = recordsProvider(t, server, nil, "")
	if _, err := p.Check(t.Context()); !errors.Is(err, host.ErrUnsupported) {
		t.Fatal("records-only peer without domains accepted", err)
	}
	p = recordsProvider(t, server, []string{"example.test"}, "upsert-only")
	checked(t, p)
	pubs, err := p.Destinations(t.Context())
	if err != nil || len(pubs) != 1 || pubs[0].Policy != plan.PolicyUpsertOnly || pubs[0].Destination != webhookDestination(server.URL) {
		t.Fatal("destination", pubs, err)
	}
	for _, name := range []string{"x.private.example.test.", "_kudurru-a.www.example.test."} {
		if _, err := p.Adjust(t.Context(), pubs[0], []public.Endpoint{endpointA(name, "192.0.2.1")}); !errors.Is(err, errInput) {
			t.Fatal("unpublishable name accepted", name, err)
		}
	}
	for _, raw := range []string{`{"endpoint":"https://peer.test","policy":"replace"}`, `{"endpoint":"https://peer.test","domains":["*.example.test"]}`, `{"endpoint":"https://peer.test","domains":["example.test","example.test."]}`} {
		var config public.WebhookIntegration
		if err := json.Unmarshal([]byte(raw), &config); err != nil {
			t.Fatal(err)
		}
		if _, err := publicationConfig(config, host.InstanceID{ClusterID: "c", UID: "u"}); err == nil {
			t.Fatal("invalid publication accepted", raw)
		}
	}
}

func TestRegistryNamesRoundTrip(t *testing.T) {
	for _, key := range []kinds.RecordKey{{Name: "www.example.test.", Type: "A"}, {Name: "*.example.test.", Type: "AAAA"}, {Name: "example.test.", Type: "TXT", SetIdentifier: "east"}} {
		name, err := registryName(key)
		if err != nil {
			t.Fatal(err)
		}
		back, ok := registryKey(name, key.SetIdentifier)
		if !ok || back != key {
			t.Fatal("registry name does not round-trip", key, name, back)
		}
	}
	if _, ok := registryOwner(public.Endpoint{RecordType: "TXT", Targets: []string{`"heritage=kudurru,kudurru/owner=short"`}}); ok {
		t.Fatal("malformed marker accepted")
	}
}
