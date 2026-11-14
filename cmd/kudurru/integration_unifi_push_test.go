package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/unifi"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// unifiPushPeer is a stateful TLS double of the documented UniFi Network
// Integration API surface (v10.6.106: info, sites, clients, networks, DNS
// policies) plus the legacy cmd/stamgr station command. No live controller
// is exercised (D401).
type unifiPushPeer struct {
	server *httptest.Server
	mu     sync.Mutex
	// policies are the site's DNS policies by ID; network is the LAN detail.
	policies map[string]map[string]any
	network  map[string]any
	next     int
	// writes lists every non-GET request as "METHOD path"; bodies holds
	// their decoded bodies in the same order.
	writes []string
	bodies []map[string]any
	// dropPost makes the next policy creation take effect and lose its
	// answer: the connection closes before any response.
	dropPost bool
	// failPostAt makes the policy creation with that sequence number (the
	// value next takes) answer 500 without creating anything.
	failPostAt int
}

const (
	unifiPushSite    = "11111111-1111-4111-8111-111111111111"
	unifiPushNetwork = "22222222-2222-4222-8222-222222222222"
	unifiPushForeign = "55555555-5555-4555-8555-555555555555"
	unifiPushOther   = "66666666-6666-4666-8666-666666666666"
)

func newUniFiPushPeer(t *testing.T) *unifiPushPeer {
	t.Helper()
	p := &unifiPushPeer{policies: map[string]map[string]any{}}
	// Two policies the operator made in UniFi; nothing may modify them.
	p.policies[unifiPushForeign] = map[string]any{"id": unifiPushForeign, "type": "A_RECORD", "enabled": true, "domain": "nas.example", "ipv4Address": "192.0.2.50", "ttlSeconds": 0, "metadata": map[string]any{"origin": "USER_DEFINED"}}
	p.policies[unifiPushOther] = map[string]any{"id": unifiPushOther, "type": "FORWARD_DOMAIN", "enabled": true, "domain": "corp.example", "ipAddress": "192.0.2.9", "metadata": map[string]any{"origin": "USER_DEFINED"}}
	p.network = map[string]any{"id": unifiPushNetwork, "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10, "metadata": map[string]any{"origin": "USER_DEFINED"},
		"ipv4Configuration": map[string]any{"hostIpAddress": "192.0.2.1", "prefixLength": 24, "dhcpConfiguration": map[string]any{"mode": "SERVER", "leaseTimeSeconds": 86400, "ipAddressRange": map[string]any{"start": "192.0.2.100", "stop": "192.0.2.200"}}}}
	p.server = httptest.NewUnstartedServer(http.HandlerFunc(p.serve(t)))
	p.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	p.server.StartTLS()
	t.Cleanup(p.server.Close)
	return p
}

func (p *unifiPushPeer) serve(t *testing.T) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		p.mu.Lock()
		defer p.mu.Unlock()
		if r.Header.Get("X-API-Key") != "test-only-private-credential" {
			t.Error("UniFi request without the original API key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		if r.Method != http.MethodGet {
			p.writes = append(p.writes, r.Method+" "+r.URL.Path)
			if r.Method != http.MethodDelete {
				if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
					t.Error("malformed UniFi write body")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
			}
			p.bodies = append(p.bodies, body)
		}
		w.Header().Set("Content-Type", "application/json")
		page := func(rows []any) {
			_ = json.NewEncoder(w).Encode(map[string]any{"offset": 0, "limit": 200, "count": len(rows), "totalCount": len(rows), "data": rows})
		}
		base := "/proxy/network/integration/v1/sites/" + unifiPushSite
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && path == "/proxy/network/integration/v1/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"applicationVersion": "10.6.106"})
		case r.Method == http.MethodGet && path == "/proxy/network/integration/v1/sites":
			page([]any{map[string]any{"id": unifiPushSite, "internalReference": "default", "name": "Default"}})
		case r.Method == http.MethodGet && path == base+"/clients":
			page([]any{})
		case r.Method == http.MethodGet && path == base+"/networks":
			page([]any{map[string]any{"id": unifiPushNetwork, "name": "LAN", "management": "GATEWAY", "enabled": true, "vlanId": 10}})
		case r.Method == http.MethodGet && path == base+"/networks/"+unifiPushNetwork:
			_ = json.NewEncoder(w).Encode(p.network)
		case r.Method == http.MethodPut && path == base+"/networks/"+unifiPushNetwork:
			if _, ok := body["id"]; ok || body["name"] != "LAN" {
				t.Error("network update carried identity or lost fields")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			body["id"] = unifiPushNetwork
			p.network = body
			_ = json.NewEncoder(w).Encode(body)
		case r.Method == http.MethodGet && path == base+"/dns/policies":
			ids := make([]string, 0, len(p.policies))
			for id := range p.policies {
				ids = append(ids, id)
			}
			slices.Sort(ids)
			rows := []any{}
			for _, id := range ids {
				rows = append(rows, p.policies[id])
			}
			page(rows)
		case r.Method == http.MethodPost && path == base+"/dns/policies":
			p.next++
			if p.next == p.failPostAt {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			id := fmt.Sprintf("44444444-4444-4444-8444-%012d", p.next)
			body["id"], body["metadata"] = id, map[string]any{"origin": "USER_DEFINED"}
			p.policies[id] = body
			if p.dropPost {
				p.dropPost = false
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case strings.HasPrefix(path, base+"/dns/policies/"):
			id := strings.TrimPrefix(path, base+"/dns/policies/")
			if _, ok := p.policies[id]; !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			switch r.Method {
			case http.MethodPut:
				body["id"] = id
				p.policies[id] = body
				_ = json.NewEncoder(w).Encode(body)
			case http.MethodDelete:
				delete(p.policies, id)
				w.WriteHeader(http.StatusNoContent)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}
		case r.Method == http.MethodPost && path == "/proxy/network/api/s/default/cmd/stamgr":
			_ = json.NewEncoder(w).Encode(map[string]any{"meta": map[string]any{"rc": "ok"}, "data": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(path, "/proxy/network/"):
			// Legacy enrichment endpoints are absent; the adapter tolerates 404.
			w.WriteHeader(http.StatusNotFound)
		default:
			t.Error("unexpected UniFi request", r.Method, path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
}

func (p *unifiPushPeer) written() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.writes)
}

func (p *unifiPushPeer) definition(name, path string) *public.Integration {
	sum := sha256.Sum256(p.server.Certificate().Raw)
	d := integrationDefinition(name, path)
	d.Spec.UniFi.Endpoint = p.server.URL
	d.Spec.UniFi.TLSFingerprint = "sha256:" + hex.EncodeToString(sum[:])
	return d
}

func unifiPushStart(t *testing.T, peer *unifiPushPeer, records []*public.DNSRecordSet, configure func(*public.Integration)) (*integrationOperationFixture, http.Handler) {
	t.Helper()
	f := &integrationOperationFixture{}
	f.s, f.path = integrationFixture(t, nil, nil)
	integrationOperationCaller(t, f)
	for _, record := range records {
		if _, err := f.s.opts.Store.Apply(t.Context(), record, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	d := peer.definition("gateway", f.path)
	configure(d)
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("UniFi writes", peer.written())
			for _, status := range f.s.integrations.host.Statuses() {
				t.Logf("integration %s/%s %+v", status.Phase, status.Reason, status.Result)
			}
		}
	})
	integrationOperationStart(t, f, d)
	return f, integrationAdminRouter(t, f)
}

func unifiPushReview(t *testing.T, f *integrationOperationFixture, router http.Handler) api.IntegrationReview {
	t.Helper()
	summary := integrationSettled(t, f, router, integrationOwner(f.s, f.d), f.d.Name)
	return api.IntegrationReview{UID: summary.UID, Revision: summary.Revision}
}

func unifiPushCall(t *testing.T, f *integrationOperationFixture, router http.Handler, path string, review api.IntegrationReview, want int) api.IntegrationExecutionResult {
	t.Helper()
	w := integrationAdminCall(t, router, f, http.MethodPost, "/integrations/default/gateway"+path, review, f.token)
	if w.Code != want {
		t.Fatalf("%s: status %d, want %d: %s", path, w.Code, want, w.Body.String())
	}
	var out api.IntegrationExecutionResult
	if want == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
	}
	return out
}

// unifiPushCapturedWithout waits until the host frame an operator command
// captures (not only the root's latest cut) no longer selects the record set
// name. The root publishes its cut before the host adopts it as the frame's
// input, so a review taken in between still checks the deleted record set.
func unifiPushCapturedWithout(t *testing.T, f *integrationOperationFixture, name string) {
	t.Helper()
	owner := integrationOwner(f.s, f.d)
	awaitServingRuntime(t, f.s, func() bool {
		var current public.Integration
		if f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Integration", Namespace: "default", Name: f.d.Name}, &current) != nil {
			return false
		}
		capture, err := f.s.integrations.host.CaptureCommand(t.Context(), owner, current.ResourceVersion)
		if err != nil {
			return false
		}
		ctx, g, err := capture.Bind(t.Context())
		if err != nil {
			return false
		}
		resource, err := f.s.integrations.host.RequestResource(ctx, g.Ledger)
		cut, ok := resource.(*integrationCut)
		if err != nil || !ok || cut == nil {
			return false
		}
		for _, source := range cut.input.Sources {
			if source.Records.Name == name {
				return false
			}
		}
		return true
	})
}

func unifiPushRecord(name, typ, target string) *public.DNSRecordSet {
	return &public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: name + ".example.", RecordType: typ, RecordTTL: 300, Targets: []string{target}}}}}
}

// With every push left at its default, inventory polling, check, observation
// sync and the action listing never send a write to the controller.
func TestIntegrationUniFiPushDefaultsNeverWrite(t *testing.T) {
	peer := newUniFiPushPeer(t)
	f, router := unifiPushStart(t, peer, nil, func(*public.Integration) {})
	review := unifiPushReview(t, f, router)
	unifiPushCall(t, f, router, "/check", review, http.StatusOK)
	review = unifiPushReview(t, f, router)
	review.Mode = "observe"
	unifiPushCall(t, f, router, "/sync", review, http.StatusOK)
	review = unifiPushReview(t, f, router)
	w := integrationAdminCall(t, router, f, http.MethodGet, "/integrations/default/gateway/actions?uid="+review.UID+"&revision="+review.Revision, nil, f.token)
	var listed api.IntegrationExecutionResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Actions) != 0 {
		t.Fatal("default UniFi instance advertised actions", w.Code, w.Body.String())
	}
	unifiPushCall(t, f, router, "/actions/"+unifi.ActionBlockClient, api.IntegrationReview{UID: review.UID, Revision: review.Revision, Params: json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)}, http.StatusNotImplemented)
	unifiPushCall(t, f, router, "/preview", review, http.StatusNotImplemented)
	if writes := peer.written(); len(writes) != 0 {
		t.Fatal("default UniFi instance wrote to the controller", writes)
	}
}

// Mirrored records are UniFi DNS policies written only by a reviewed sync:
// the preview is exact, one apply creates each policy once, a repeat is a
// no-op, drift of an owned policy is previewed and repaired, a removed record
// deletes only its own policy, and operator policies are never modified.
func TestIntegrationUniFiPushMirrorRecordsReviewed(t *testing.T) {
	peer := newUniFiPushPeer(t)
	records := []*public.DNSRecordSet{unifiPushRecord("web", "A", "192.0.2.80"), unifiPushRecord("nas", "A", "192.0.2.81"), unifiPushRecord("alias", "CNAME", "web.example."), unifiPushRecord("corp", "A", "192.0.2.82")}
	f, router := unifiPushStart(t, peer, records, func(d *public.Integration) {
		d.Spec.UniFi.Push.MirrorRecords = public.UniFiRecordMirror{Enabled: true, Domains: []string{"example"}}
	})
	owner := integrationOwner(f.s, f.d)
	// Polling and reconcile ticks of a reviewed-only publisher apply nothing.
	if _, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err != nil {
		t.Fatal("unreviewed tick failed", err)
	}
	if writes := peer.written(); len(writes) != 0 {
		t.Fatal("polling published without review", writes)
	}
	review := unifiPushReview(t, f, router)
	preview := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	var created []string
	for _, op := range preview.Operations {
		created = append(created, op.Action+" "+op.Name+" "+op.Type)
	}
	slices.Sort(created)
	if !slices.Equal(created, []string{"create alias.example. CNAME", "create web.example. A"}) || len(preview.Conflicts) != 2 {
		t.Fatalf("mirror preview: %v conflicts %+v", created, preview.Conflicts)
	}
	// A different digest is refused before any write.
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", strings.Repeat("a", 64)
	unifiPushCall(t, f, router, "/sync", review, http.StatusConflict)
	if writes := peer.written(); len(writes) != 0 {
		t.Fatal("stale review reached the controller", writes)
	}
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", preview.Digest
	result := unifiPushCall(t, f, router, "/sync", review, http.StatusOK)
	writes := peer.written()
	if !result.Completed || result.Applied != 2 || len(writes) != 2 || writes[0] != "POST /proxy/network/integration/v1/sites/"+unifiPushSite+"/dns/policies" || writes[1] != writes[0] {
		t.Fatalf("reviewed mirror did not create each policy once: %+v %v", result, writes)
	}
	// A repeat preview is empty and its apply sends nothing.
	review = unifiPushReview(t, f, router)
	again := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(again.Operations) != 0 || again.Unchanged != 2 {
		t.Fatalf("mirror not idempotent: %+v", again)
	}
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", again.Digest
	if result = unifiPushCall(t, f, router, "/sync", review, http.StatusOK); result.Applied != 0 || len(peer.written()) != 2 {
		t.Fatal("repeat apply wrote", result, peer.written())
	}
	// Drift of an owned policy shows as an update and is repaired once.
	var web, alias string
	peer.mu.Lock()
	for id, policy := range peer.policies {
		switch policy["domain"] {
		case "web.example":
			web = id
			policy["ipv4Address"] = "192.0.2.99"
		case "alias.example":
			alias = id
		}
	}
	peer.mu.Unlock()
	review = unifiPushReview(t, f, router)
	drift := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(drift.Operations) != 1 || drift.Operations[0].Action != "update" || drift.Operations[0].Before.Targets[0] != "192.0.2.99" {
		t.Fatalf("drift not reported: %+v", drift)
	}
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", drift.Digest
	unifiPushCall(t, f, router, "/sync", review, http.StatusOK)
	if writes = peer.written(); len(writes) != 3 || writes[2] != "PUT /proxy/network/integration/v1/sites/"+unifiPushSite+"/dns/policies/"+web {
		t.Fatal("drift repair", writes)
	}
	// Removing a record deletes only the policy this instance created.
	if err := f.s.opts.Store.Delete(t.Context(), store.Key{Kind: "DNSRecordSet", Namespace: "default", Name: "alias"}, store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	// Review only once the frame a command captures no longer lists it.
	unifiPushCapturedWithout(t, f, "alias")
	review = unifiPushReview(t, f, router)
	removal := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(removal.Operations) != 1 || removal.Operations[0].Action != "delete" {
		t.Fatalf("removal preview: %+v", removal)
	}
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", removal.Digest
	unifiPushCall(t, f, router, "/sync", review, http.StatusOK)
	if writes = peer.written(); len(writes) != 4 || writes[3] != "DELETE /proxy/network/integration/v1/sites/"+unifiPushSite+"/dns/policies/"+alias {
		t.Fatal("removal", writes)
	}
	for _, w := range writes {
		if strings.HasSuffix(w, unifiPushForeign) || strings.HasSuffix(w, unifiPushOther) {
			t.Fatal("an operator policy was modified", w)
		}
	}
	peer.mu.Lock()
	foreign := peer.policies[unifiPushForeign]
	peer.mu.Unlock()
	if foreign["ipv4Address"] != "192.0.2.50" {
		t.Fatal("operator policy changed")
	}
	syncs := 0
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.sync" && entry.Result == sqlite.AuditSuccess {
			syncs++
		}
	}
	if syncs != 4 {
		t.Fatal("reviewed syncs were not audited", syncs)
	}
}

// The DHCP DNS server push and the client actions are advertised only when
// enabled, need integrations.act, preview before they write, apply once
// under the reviewed digest, report drift, and are audited.
func TestIntegrationUniFiPushActionsReviewed(t *testing.T) {
	peer := newUniFiPushPeer(t)
	f, router := unifiPushStart(t, peer, nil, func(d *public.Integration) {
		d.Spec.UniFi.Legacy = true
		d.Spec.UniFi.Push = public.UniFiPush{DHCPDNSServers: true, DNSServers: []string{"192.0.2.53", "192.0.2.54"}, Networks: []string{"LAN"}, ClientActions: true}
	})
	review := unifiPushReview(t, f, router)
	w := integrationAdminCall(t, router, f, http.MethodGet, "/integrations/default/gateway/actions?uid="+review.UID+"&revision="+review.Revision, nil, f.token)
	var listed api.IntegrationExecutionResult
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listed) != nil {
		t.Fatal("action listing", w.Code)
	}
	var names []string
	for _, a := range listed.Actions {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{unifi.ActionDHCPDNSServers, unifi.ActionBlockClient, unifi.ActionUnblockClient}) {
		t.Fatal("advertised actions", names)
	}
	act := func(name, params string, want int) api.IntegrationExecutionResult {
		t.Helper()
		r := unifiPushReview(t, f, router)
		r.Params = json.RawMessage(params)
		return unifiPushCall(t, f, router, "/actions/"+name, r, want)
	}
	plan := func(result api.IntegrationExecutionResult) unifi.DHCPPlan {
		t.Helper()
		var p unifi.DHCPPlan
		if err := json.Unmarshal(result.Result, &p); err != nil {
			t.Fatal("action result", err)
		}
		return p
	}
	preview := plan(act(unifi.ActionDHCPDNSServers, `{"mode":"preview"}`, http.StatusOK))
	if len(preview.Changes) != 1 || preview.Changes[0].Action != "update" || len(preview.Changes[0].Current) != 0 || !slices.Equal(preview.Changes[0].Desired, []string{"192.0.2.53", "192.0.2.54"}) || !kinds.ValidDigest(preview.Digest) {
		t.Fatalf("DHCP preview: %+v", preview)
	}
	act(unifi.ActionDHCPDNSServers, `{"mode":"apply","digest":"`+strings.Repeat("b", 64)+`"}`, http.StatusConflict)
	if writes := peer.written(); len(writes) != 0 {
		t.Fatal("preview or stale apply wrote", writes)
	}
	applied := plan(act(unifi.ActionDHCPDNSServers, `{"mode":"apply","digest":"`+preview.Digest+`"}`, http.StatusOK))
	writes := peer.written()
	if applied.Applied != 1 || len(writes) != 1 || writes[0] != "PUT /proxy/network/integration/v1/sites/"+unifiPushSite+"/networks/"+unifiPushNetwork {
		t.Fatalf("DHCP apply: %+v %v", applied, writes)
	}
	peer.mu.Lock()
	sent := peer.bodies[0]
	peer.mu.Unlock()
	dhcp := sent["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)
	if fmt.Sprint(dhcp["dnsServerIpAddressesOverride"]) != "[192.0.2.53 192.0.2.54]" || dhcp["leaseTimeSeconds"] == nil || sent["vlanId"] == nil {
		t.Fatalf("network body: %v", sent)
	}
	again := plan(act(unifi.ActionDHCPDNSServers, `{"mode":"preview"}`, http.StatusOK))
	if again.Changes[0].Action != "unchanged" || again.Unchanged != 1 {
		t.Fatalf("DHCP not idempotent: %+v", again)
	}
	// A repeat apply under the fresh digest sends no PUT.
	if repeat := plan(act(unifi.ActionDHCPDNSServers, `{"mode":"apply","digest":"`+again.Digest+`"}`, http.StatusOK)); repeat.Applied != 0 || len(peer.written()) != 1 {
		t.Fatalf("repeat DHCP apply wrote: %+v %v", repeat, peer.written())
	}
	// Someone changes the servers back in UniFi: the preview shows it.
	peer.mu.Lock()
	peer.network["ipv4Configuration"].(map[string]any)["dhcpConfiguration"].(map[string]any)["dnsServerIpAddressesOverride"] = []any{"192.0.2.1"}
	peer.mu.Unlock()
	if drift := plan(act(unifi.ActionDHCPDNSServers, `{"mode":"preview"}`, http.StatusOK)); drift.Changes[0].Action != "update" || !slices.Equal(drift.Changes[0].Current, []string{"192.0.2.1"}) {
		t.Fatalf("DHCP drift: %+v", drift)
	}
	if len(peer.written()) != 1 {
		t.Fatal("drift preview wrote")
	}
	// One client in two letter cases: both rows name it the same way (D384).
	act(unifi.ActionBlockClient, `{"mac":"02:ab:cd:00:00:01"}`, http.StatusOK)
	act(unifi.ActionUnblockClient, `{"mac":"02:AB:CD:00:00:01"}`, http.StatusOK)
	act(unifi.ActionBlockClient, `{"mac":"not-a-mac"}`, http.StatusBadRequest)
	writes = peer.written()
	peer.mu.Lock()
	commands := []any{peer.bodies[1]["cmd"], peer.bodies[2]["cmd"]}
	peer.mu.Unlock()
	if len(writes) != 3 || writes[1] != "POST /proxy/network/api/s/default/cmd/stamgr" || commands[0] != "block-sta" || commands[1] != "unblock-sta" {
		t.Fatal("client actions", writes, commands)
	}
	acts, clients := 0, 0
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.act" && entry.Result == sqlite.AuditSuccess && strings.Contains(entry.Reason, "with params sha256:") {
			acts++
		}
		if entry.Action == "integration.act" && (strings.Contains(entry.Reason, "reviewed action "+unifi.ActionBlockClient+" for client 02:ab:cd:00:00:01 with params") || strings.Contains(entry.Reason, "reviewed action "+unifi.ActionUnblockClient+" for client 02:ab:cd:00:00:01 with params")) {
			clients++
		}
		if strings.Contains(entry.Reason, "AB:CD") || strings.Contains(entry.Reason, "dhcp-dns-servers for ") {
			t.Fatal("an audit row names a client in its asked case, or names a subject for the DHCP action", entry.Reason)
		}
	}
	if acts != 7 || clients != 2 {
		t.Fatal("actions were not audited with their parameters and client", acts, clients)
	}
	// Without integrations.act no action reaches the controller.
	r := unifiPushReview(t, f, router)
	r.Params = json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)
	if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var token internalv1.APIToken
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: f.tokenID}, &token); err != nil {
			return err
		}
		token.Spec.Scopes = []string{"viewer"}
		_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: token.ResourceVersion})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	integrationAdminSettled(t, f)
	if w := integrationAdminCall(t, router, f, http.MethodPost, "/integrations/default/gateway/actions/"+unifi.ActionBlockClient, r, f.token); w.Code != http.StatusForbidden || len(peer.written()) != 3 {
		t.Fatal("action without integrations.act", w.Code, peer.written())
	}
}

// An unacknowledged policy creation is an explicit uncertain outcome: the
// answer says so, nothing is replayed (not by the API, not by polling), the
// policy it may have made is never treated as owned, and the intent settles
// without any write once the controller shows the baseline again.
func TestIntegrationUniFiPushMirrorUncertainCreate(t *testing.T) {
	peer := newUniFiPushPeer(t)
	f, router := unifiPushStart(t, peer, []*public.DNSRecordSet{unifiPushRecord("web", "A", "192.0.2.80")}, func(d *public.Integration) {
		d.Spec.UniFi.Push.MirrorRecords = public.UniFiRecordMirror{Enabled: true, Domains: []string{"example"}, Types: []string{"A"}}
	})
	owner := integrationOwner(f.s, f.d)
	review := unifiPushReview(t, f, router)
	preview := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(preview.Operations) != 1 {
		t.Fatalf("preview: %+v", preview)
	}
	peer.mu.Lock()
	peer.dropPost = true
	peer.mu.Unlock()
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", preview.Digest
	w := integrationAdminCall(t, router, f, http.MethodPost, "/integrations/default/gateway/sync", review, f.token)
	var refusal struct {
		Error struct {
			Key  string                         `json:"key"`
			Hint api.IntegrationExecutionResult `json:"hint"`
		} `json:"error"`
	}
	if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &refusal) != nil || refusal.Error.Key != "integration_uncertain" || !refusal.Error.Hint.Uncertain || refusal.Error.Hint.Pending != 1 {
		t.Fatal("lost acknowledgment was not uncertain", w.Code, w.Body.String())
	}
	// Polling neither replays the create nor adopts the policy it may have made.
	unifiPushReview(t, f, router)
	// The pending intent is reported and the inventory observation still runs.
	if result, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err != nil || result == nil || result.Pending != 1 || result.Source == nil {
		t.Fatal("an unproven policy settled the pending intent, or polling stopped observing", err, result)
	}
	if writes := peer.written(); len(writes) != 1 {
		t.Fatal("uncertain create replayed", writes)
	}
	var stray string
	peer.mu.Lock()
	for id, policy := range peer.policies {
		if policy["domain"] == "web.example" {
			stray = id
		}
	}
	delete(peer.policies, stray)
	peer.mu.Unlock()
	// The operator removed it in UniFi: the next tick settles without a write.
	unifiPushReview(t, f, router)
	if result, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err != nil || result == nil || result.Pending != 0 {
		t.Fatal("baseline did not settle the pending intent", err, result)
	}
	if writes := peer.written(); len(writes) != 1 || stray == "" {
		t.Fatal("settling wrote", writes)
	}
	review = unifiPushReview(t, f, router)
	again := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(again.Operations) != 1 || again.Operations[0].Action != "create" {
		t.Fatalf("settled intent did not return to review: %+v", again)
	}
	uncertain := 0
	for _, entry := range integrationAudit(t, f.s, f.d) {
		if entry.Action == "integration.sync" && entry.Result == sqlite.AuditError {
			uncertain++
		}
	}
	if uncertain != 1 {
		t.Fatal("uncertain sync was not audited", uncertain)
	}
}

// A multi-address record whose second policy creation fails after the first
// was acknowledged is uncertain, and it does not stay pending forever: once
// the operator removes the acknowledged policy, so that the site shows the
// baseline and no acknowledged ID exists, a poll settles the intent without
// a write and the record returns to review.
func TestIntegrationUniFiPushMirrorPartialCreate(t *testing.T) {
	peer := newUniFiPushPeer(t)
	record := unifiPushRecord("web", "A", "192.0.2.80")
	record.Spec.Endpoints[0].Targets = append(record.Spec.Endpoints[0].Targets, "192.0.2.81")
	f, router := unifiPushStart(t, peer, []*public.DNSRecordSet{record}, func(d *public.Integration) {
		d.Spec.UniFi.Push.MirrorRecords = public.UniFiRecordMirror{Enabled: true, Domains: []string{"example"}, Types: []string{"A"}}
	})
	owner := integrationOwner(f.s, f.d)
	review := unifiPushReview(t, f, router)
	preview := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(preview.Operations) != 1 || preview.Operations[0].Action != "create" {
		t.Fatalf("preview: %+v", preview)
	}
	peer.mu.Lock()
	peer.failPostAt = peer.next + 2
	peer.mu.Unlock()
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", preview.Digest
	if w := integrationAdminCall(t, router, f, http.MethodPost, "/integrations/default/gateway/sync", review, f.token); w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), `"pending":1`) {
		t.Fatal("partial create was not uncertain", w.Code, w.Body.String())
	}
	if writes := peer.written(); len(writes) != 2 {
		t.Fatal("partial create", writes)
	}
	// While the acknowledged policy exists, polling keeps the intent pending.
	unifiPushReview(t, f, router)
	if result, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err != nil || result == nil || result.Pending != 1 {
		t.Fatal("an acknowledged policy did not keep the intent pending", err, result)
	}
	var created []string
	peer.mu.Lock()
	for id, policy := range peer.policies {
		if policy["domain"] == "web.example" {
			created = append(created, id)
		}
	}
	for _, id := range created {
		delete(peer.policies, id)
	}
	peer.mu.Unlock()
	if len(created) != 1 {
		t.Fatal("acknowledged policies", created)
	}
	// The site is back at its baseline: the next poll settles without a write.
	unifiPushReview(t, f, router)
	if result, err := f.s.integrations.host.Sync(t.Context(), owner, ""); err != nil || result == nil || result.Pending != 0 {
		t.Fatal("baseline did not settle the partial intent", err, result)
	}
	if writes := peer.written(); len(writes) != 2 {
		t.Fatal("settling wrote", writes)
	}
	review = unifiPushReview(t, f, router)
	again := unifiPushCall(t, f, router, "/preview", review, http.StatusOK).Preview
	if len(again.Operations) != 1 || again.Operations[0].Action != "create" || len(again.Operations[0].After.Targets) != 2 {
		t.Fatalf("settled intent did not return to review: %+v", again)
	}
	review = unifiPushReview(t, f, router)
	review.Mode, review.PreviewDigest = "publish", again.Digest
	if result := unifiPushCall(t, f, router, "/sync", review, http.StatusOK); result.Applied != 1 || len(peer.written()) != 4 {
		t.Fatal("reviewed retry", result, peer.written())
	}
}

// The act audit digest is over canonical parameters: the same request with
// other whitespace or key order audits the same digest.
func TestIntegrationParamsDigestCanonical(t *testing.T) {
	a := integrationParamsDigest(json.RawMessage(`{"mode":"apply","digest":"x"}`))
	b := integrationParamsDigest(json.RawMessage(" {\n \"digest\" : \"x\", \"mode\":\"apply\"}\n"))
	if a != b || a != kinds.Digest([]byte(`{"digest":"x","mode":"apply"}`)) {
		t.Fatal("parameter digest is not canonical", a, b)
	}
	if integrationParamsDigest(json.RawMessage(`{"mac":"00:11:22:33:44:55"}`)) == integrationParamsDigest(json.RawMessage(`{"mac":"00:11:22:33:44:56"}`)) {
		t.Fatal("different parameters share a digest")
	}
	if raw := json.RawMessage(`{"a":1} {}`); integrationParamsDigest(raw) != kinds.Digest(raw) {
		t.Fatal("trailing data was canonicalized")
	}
}
