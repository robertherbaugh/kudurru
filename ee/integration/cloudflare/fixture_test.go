package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

const zoneID = "11111111111111111111111111111111"
const accountID = "22222222222222222222222222222222"
const testToken = "test-only-token-do-not-log"

type contextKey struct{}

type fakeHost struct {
	mu              sync.Mutex
	owner           host.InstanceID
	calls           int
	writes          int
	failAfterWrites int
	requireContext  bool
}

func (h *fakeHost) IntegrationOwner() host.InstanceID { return h.owner }
func (*fakeHost) Logger() *slog.Logger                { return slog.New(slog.NewTextHandler(io.Discard, nil)) }
func (*fakeHost) Metrics() prometheus.Registerer      { return prometheus.NewRegistry() }
func (*fakeHost) Resolve(context.Context, dns.Question) (*dns.Msg, error) {
	return nil, host.ErrUnsupported
}
func (*fakeHost) SetCondition(context.Context, string, string, string, bool) error { return nil }
func (h *fakeHost) Request(ctx context.Context, provider, account string, mutating bool, fn func(context.Context) error) error {
	h.mu.Lock()
	if provider != "cloudflare" || account != "all" || h.requireContext && ctx.Value(contextKey{}) != "original" {
		h.mu.Unlock()
		return host.ErrFenced
	}
	if h.failAfterWrites > 0 && h.writes >= h.failAfterWrites {
		h.mu.Unlock()
		return host.ErrFenced
	}
	h.calls++
	if mutating {
		h.writes++
	}
	h.mu.Unlock()
	return fn(ctx)
}

type secretFunc func(context.Context, string) (string, error)

func (f secretFunc) Resolve(ctx context.Context, name string) (string, error) { return f(ctx, name) }

type fixture struct {
	t           *testing.T
	mu          sync.Mutex
	server      *httptest.Server
	p           *Provider
	h           *fakeHost
	pub         host.Publication
	ctx         context.Context
	rows        map[string]map[string]any
	next        int
	requests    int
	batches     []map[string][]map[string]any
	tokenStatus string
	zoneName    string
	planName    string
	hook        func(http.ResponseWriter, *http.Request) bool // called with mu held
}

func newFixture(t *testing.T, zones ...public.CloudflareZone) *fixture {
	t.Helper()
	f := &fixture{t: t, rows: map[string]map[string]any{}, next: 100, tokenStatus: "active", zoneName: "example.com", planName: "Free Website", ctx: context.WithValue(context.Background(), contextKey{}, "original")}
	f.h = &fakeHost{owner: host.InstanceID{ClusterID: "cluster", Namespace: "dns", Name: "publisher", UID: "uid-original"}, requireContext: true}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	p := New()
	p.apiURL = f.server.URL + "/client/v4/"
	p.cadence = 0
	f.p = p
	if len(zones) == 0 {
		zones = []public.CloudflareZone{{Zone: "example.com", ZoneID: zoneID}}
	}
	raw, err := json.Marshal(public.CloudflareIntegration{Zones: zones})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Provision(f.ctx, f.h, raw, secretFunc(func(_ context.Context, name string) (string, error) {
		if name != "apiToken" {
			t.Errorf("unexpected secret capability %q", name)
		}
		return testToken, nil
	})); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := p.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err = p.Validate(f.ctx); err != nil {
		t.Fatal(err)
	}
	pubs, err := p.Destinations(f.ctx)
	if err != nil || len(pubs) != 1 {
		t.Fatal(pubs, err)
	}
	f.pub = pubs[0]
	return f
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func success(w http.ResponseWriter, result any) {
	writeJSON(w, map[string]any{"success": true, "errors": []any{}, "messages": []any{}, "result": result})
}
func (f *fixture) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if r.Header.Get("Authorization") != "Bearer "+testToken || r.Header.Get("X-Auth-Key") != "" || r.Header.Get("X-Auth-Email") != "" || r.Header.Get("X-Auth-User-Service-Key") != "" {
		f.t.Error("missing explicit token or legacy auth sent")
	}
	if f.hook != nil && f.hook(w, r) {
		return
	}
	switch r.URL.Path {
	case "/client/v4/user/tokens/verify":
		success(w, map[string]any{"id": accountID, "status": f.tokenStatus})
	case "/client/v4/zones/" + zoneID:
		success(w, map[string]any{"id": zoneID, "name": f.zoneName, "account": map[string]any{"id": accountID}, "plan": map[string]any{"name": f.planName}})
	case "/client/v4/zones":
		z := map[string]any{"id": zoneID, "name": f.zoneName, "account": map[string]any{"id": accountID}, "plan": map[string]any{"name": f.planName}}
		writeJSON(w, map[string]any{"success": true, "result": []any{z}, "result_info": map[string]any{"page": 1, "per_page": 50, "count": 1, "total_count": 1, "total_pages": 1}})
	case "/client/v4/zones/" + zoneID + "/dns_records":
		ids := []string{}
		for id := range f.rows {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("per_page"))
		if page < 1 || size != pageSize {
			f.t.Error("paging params", r.URL.RawQuery)
			w.WriteHeader(400)
			return
		}
		start := min((page-1)*size, len(ids))
		end := min(start+size, len(ids))
		rows := []any{}
		for _, id := range ids[start:end] {
			rows = append(rows, f.rows[id])
		}
		writeJSON(w, map[string]any{"success": true, "result": rows, "result_info": map[string]any{"page": page, "per_page": size, "count": len(rows), "total_count": len(ids), "total_pages": max(1, (len(ids)+size-1)/size)}})
	case "/client/v4/zones/" + zoneID + "/dns_records/batch":
		if r.Method != http.MethodPost {
			f.t.Error("wrong batch method")
			w.WriteHeader(400)
			return
		}
		var batch map[string][]map[string]any
		if json.NewDecoder(r.Body).Decode(&batch) != nil {
			f.t.Error("invalid batch")
			w.WriteHeader(400)
			return
		}
		f.batches = append(f.batches, batch)
		result := f.applyBatch(batch)
		success(w, result)
	default:
		f.t.Error("unexpected SDK path", r.URL.Path)
		w.WriteHeader(404)
	}
}
func (f *fixture) applyBatch(batch map[string][]map[string]any) map[string]any {
	result := map[string]any{}
	size := 0
	for _, kind := range []string{"deletes", "patches", "puts", "posts"} {
		list := []any{}
		for _, body := range batch[kind] {
			size++
			id, _ := body["id"].(string)
			if kind == "deletes" {
				delete(f.rows, id)
				list = append(list, map[string]any{"id": id})
				continue
			}
			if kind == "posts" {
				f.next++
				id = fmt.Sprintf("%032x", f.next)
			}
			row := map[string]any{}
			for key, value := range body {
				row[key] = value
			}
			row["id"] = id
			if kind == "patches" {
				for key, value := range f.rows[id] {
					if _, ok := row[key]; !ok {
						row[key] = value
					}
				}
			}
			f.rows[id] = row
			list = append(list, row)
		}
		result[kind] = list
	}
	if size > 200 {
		f.t.Error("oversized batch", size)
	}
	return result
}
func (f *fixture) add(name, typ, target string, ttl int64, comment string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.next++
	id := fmt.Sprintf("%032x", f.next)
	row := map[string]any{"id": id, "name": strings.TrimSuffix(name, "."), "type": typ, "content": target, "ttl": ttl, "comment": comment}
	if proxyType(typ) {
		row["proxied"] = false
	}
	f.rows[id] = row
	return id
}
func (f *fixture) snapshot() host.RemoteSnapshot {
	f.t.Helper()
	s, err := f.p.Records(f.ctx, f.pub.Destination)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}
func (f *fixture) adjusted(e public.Endpoint) public.Endpoint {
	f.t.Helper()
	out, err := f.p.Adjust(f.ctx, f.pub, []public.Endpoint{e})
	if err != nil {
		f.t.Fatal(err)
	}
	return out[0]
}
func (f *fixture) intent(desired ...public.Endpoint) *kinds.IntegrationOperation {
	f.t.Helper()
	snapshot := f.snapshot()
	in := plan.Input{Owner: f.h.owner, Destination: f.pub.Destination, ConfigDigest: kinds.Digest([]byte("config")), CredentialDigest: kinds.Digest([]byte("credentials")), SourceDigest: kinds.Digest([]byte("source")), Policy: f.pub.Policy, AdoptUnowned: f.pub.AdoptUnowned, AdoptExternalDNSOwner: f.pub.AdoptExternalDNSOwner, Desired: desired, Current: snapshot.Records, DesiredComplete: true, CurrentComplete: true}
	p, err := plan.Build(in)
	if err != nil || len(p.Conflicts) != 0 || len(p.Operations) != 1 {
		f.t.Fatalf("plan %+v: %v", p, err)
	}
	again, err := plan.Build(in)
	if err != nil || again.Digest != p.Digest {
		f.t.Fatal("preview nondeterministic", err)
	}
	op := p.Intent(0, time.Now().Add(-time.Second))
	if err = op.Validate(); err != nil {
		f.t.Fatal(err)
	}
	return op
}
func endpoint(targets ...string) public.Endpoint {
	return public.Endpoint{DNSName: "www.example.com.", RecordType: "A", RecordTTL: 300, Targets: targets}
}
