package externaldns

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var sharedValidator = sync.OnceValues(validator.New)

func newTestServer(t *testing.T) (*Server, *memory.Store) {
	t.Helper()
	v, err := sharedValidator()
	if err != nil {
		t.Fatal(err)
	}
	st := memory.New()
	for _, z := range []*v1alpha1.DNSZone{
		zone("example", "example.com.", nil),
		zone("apps", "apps.example.com.", nil),
		zone("internal", "corp.internal.", map[string]string{"external-dns": "no"}),
	} {
		if err := v.Validate(context.Background(), z, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Apply(context.Background(), z, store.ApplyOptions{Manager: "api"}); err != nil {
			t.Fatal(err)
		}
	}
	fwd := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "root"}, Spec: v1alpha1.DNSZoneSpec{ZoneName: ".", Type: v1alpha1.ZoneTypeForward}}
	if _, err := st.Apply(context.Background(), fwd, store.ApplyOptions{Manager: "bootstrap"}); err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{
		Store: st, Validator: v, Namespace: "external-dns",
		ZoneSelector: nil, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, st
}

func zone(name, apex string, labels map[string]string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: labels},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: apex, Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1." + apex}},
	}
}

// do sends a request the way the external-dns webhook provider does
// (sigs.k8s.io/external-dns provider/webhook): Accept on GET /, the media
// type as Content-Type on POSTs.
func do(t *testing.T, h http.Handler, method, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == http.MethodGet {
		r.Header.Set("Accept", MediaType)
	} else {
		r.Header.Set("Content-Type", MediaType)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestNegotiate(t *testing.T) {
	s, _ := newTestServer(t)
	w := do(t, s, http.MethodGet, "/", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	// external-dns refuses any other Content-Type, byte for byte.
	if ct := w.Header().Get("Content-Type"); ct != MediaType {
		t.Fatalf("content type %q", ct)
	}
	if got := strings.TrimSpace(w.Body.String()); got != `{"include":["apps.example.com","corp.internal","example.com"]}` {
		t.Fatalf("domain filter %s", got)
	}
	// A zone selector narrows the filter.
	s.opts.ZoneSelector = map[string]string{"external-dns": "no"}
	w = do(t, s, http.MethodGet, "/", "")
	if got := strings.TrimSpace(w.Body.String()); got != `{"include":["corp.internal"]}` {
		t.Fatalf("selected filter %s", got)
	}
}

// changes renders plan.Changes as external-dns v0.15+ marshals it.
const createBody = `{"create":[
 {"dnsName":"web.example.com","targets":["192.0.2.10"],"recordType":"A","recordTTL":300,"labels":{"owner":"default","resource":"ingress/default/web"}},
 {"dnsName":"web.example.com","targets":["\"heritage=external-dns,external-dns/owner=default,external-dns/resource=ingress/default/web\""],"recordType":"TXT"},
 {"dnsName":"*.apps.example.com","targets":["lb.example.com"],"recordType":"CNAME","providerSpecific":[{"name":"alias","value":"false"}]}
]}`

func TestApplyAndRecords(t *testing.T) {
	s, st := newTestServer(t)
	if w := do(t, s, http.MethodPost, "/records", createBody); w.Code != http.StatusNoContent {
		t.Fatalf("apply: %d %s", w.Code, w.Body.String())
	}
	w := do(t, s, http.MethodGet, "/records", "")
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != MediaType {
		t.Fatalf("records: %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	var eps []Endpoint
	if err := json.Unmarshal(w.Body.Bytes(), &eps); err != nil {
		t.Fatal(err)
	}
	if len(eps) != 3 {
		t.Fatalf("endpoints %+v", eps)
	}
	byKey := map[string]Endpoint{}
	for _, e := range eps {
		byKey[e.RecordType+" "+e.DNSName] = e
	}
	txt := byKey["TXT web.example.com"]
	if len(txt.Targets) != 1 || !strings.HasPrefix(txt.Targets[0], `"heritage=external-dns`) {
		t.Fatalf("ownership TXT not stored verbatim: %+v", txt)
	}
	if a := byKey["A web.example.com"]; a.RecordTTL != 300 || a.Labels["owner"] != "default" {
		t.Fatalf("A endpoint %+v", a)
	}
	if c := byKey["CNAME *.apps.example.com"]; len(c.ProviderSpecific) != 1 || c.ProviderSpecific[0].Name != "alias" {
		t.Fatalf("CNAME endpoint %+v", c)
	}

	// One record set per DNS name, managed by the integration, in the
	// configured namespace, referencing the longest matching zone.
	list, err := st.List(context.Background(), "DNSRecordSet", store.ListOptions{Namespace: "external-dns"})
	if err != nil || len(list.Items) != 2 {
		t.Fatalf("record sets %v %d", err, len(list.Items))
	}
	for _, o := range list.Items {
		rs := o.(*v1alpha1.DNSRecordSet)
		if store.ManagedBy(rs) != "integration:external-dns" {
			t.Errorf("%s managed by %q", rs.Name, store.ManagedBy(rs))
		}
		switch rs.Name {
		case "edns-web.example.com":
			if rs.Spec.ZoneRef.Name != "example" || len(rs.Spec.Endpoints) != 2 {
				t.Errorf("web record set %+v", rs.Spec)
			}
		default:
			if !strings.HasPrefix(rs.Name, "edns-wildcard.apps.example.com-") || rs.Spec.ZoneRef.Name != "apps" {
				t.Errorf("wildcard record set %s %+v", rs.Name, rs.Spec)
			}
		}
	}

	// Update: v0.14-style capitalised keys work too.
	update := `{"UpdateOld":[{"dnsName":"web.example.com","targets":["192.0.2.10"],"recordType":"A","recordTTL":300}],
	"UpdateNew":[{"dnsName":"web.example.com","targets":["192.0.2.11","192.0.2.12"],"recordType":"A","recordTTL":60}]}`
	if w := do(t, s, http.MethodPost, "/records", update); w.Code != http.StatusNoContent {
		t.Fatalf("update: %d %s", w.Code, w.Body.String())
	}
	var rs v1alpha1.DNSRecordSet
	if err := st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: "external-dns", Name: "edns-web.example.com"}, &rs); err != nil {
		t.Fatal(err)
	}
	if len(rs.Spec.Endpoints) != 2 || rs.Spec.Endpoints[1].RecordTTL != 60 || len(rs.Spec.Endpoints[1].Targets) != 2 {
		t.Fatalf("after update %+v", rs.Spec.Endpoints)
	}

	// Delete everything of web.example.com: the record set goes away.
	del := `{"delete":[{"dnsName":"web.example.com","recordType":"A","targets":["192.0.2.11","192.0.2.12"]},
	{"dnsName":"web.example.com","recordType":"TXT","targets":["x"]}]}`
	if w := do(t, s, http.MethodPost, "/records", del); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body.String())
	}
	err = st.Get(context.Background(), store.Key{Kind: "DNSRecordSet", Namespace: "external-dns", Name: "edns-web.example.com"}, &rs)
	if err == nil {
		t.Fatal("empty record set not deleted")
	}
}

func TestApplyErrors(t *testing.T) {
	s, st := newTestServer(t)
	cases := []struct {
		name string
		body string
		code int
	}{
		{"bad json", `{"create":[`, http.StatusBadRequest},
		{"no dnsName", `{"create":[{"recordType":"A","targets":["192.0.2.1"]}]}`, http.StatusBadRequest},
		{"outside every zone", `{"create":[{"dnsName":"x.elsewhere.org","recordType":"A","targets":["192.0.2.1"]}]}`, http.StatusBadRequest},
		{"invalid endpoint", `{"create":[{"dnsName":"bad.example.com","recordType":"a b","targets":["192.0.2.1"]}]}`, http.StatusUnprocessableEntity},
	}
	for _, tc := range cases {
		if w := do(t, s, http.MethodPost, "/records", tc.body); w.Code != tc.code {
			t.Errorf("%s: %d, want %d (%s)", tc.name, w.Code, tc.code, w.Body.String())
		}
	}
	// A record set of the same name owned by someone else is not taken over.
	other := &v1alpha1.DNSRecordSet{
		ObjectMeta: metav1.ObjectMeta{Name: "edns-mine.example.com", Namespace: "external-dns"},
		Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: "example"}, Endpoints: []v1alpha1.Endpoint{{DNSName: "other.example.com", RecordType: "A", Targets: []string{"192.0.2.9"}}}},
	}
	if _, err := st.Apply(context.Background(), other, store.ApplyOptions{Manager: "api"}); err != nil {
		t.Fatal(err)
	}
	w := do(t, s, http.MethodPost, "/records", `{"create":[{"dnsName":"mine.example.com","recordType":"A","targets":["192.0.2.1"]}]}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("managed_by conflict: %d %s", w.Code, w.Body.String())
	}
	// Record sets of other managers are invisible to external-dns.
	if w := do(t, s, http.MethodGet, "/records", ""); strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("records %s", w.Body.String())
	}
}

func TestBodyCap(t *testing.T) {
	s, _ := newTestServer(t)
	big := bytes.Repeat([]byte(" "), MaxBodyBytes+10)
	r := httptest.NewRequest(http.MethodPost, "/records", bytes.NewReader(big))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d", w.Code)
	}
}

func TestAdjustEndpoints(t *testing.T) {
	s, _ := newTestServer(t)
	body := `[{"dnsName":"a.example.com","targets":["192.0.2.1"],"recordType":"A"}]`
	w := do(t, s, http.MethodPost, "/adjustendpoints", body)
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != MediaType {
		t.Fatalf("adjust %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if strings.TrimSpace(w.Body.String()) != body {
		t.Fatalf("not echoed: %s", w.Body.String())
	}
	if w := do(t, s, http.MethodGet, "/adjustendpoints", ""); w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET adjustendpoints: %d", w.Code)
	}
}

func TestRecordSetName(t *testing.T) {
	cases := map[string]string{
		"web.example.com":   "edns-web.example.com",
		"WEB.Example.com.":  "edns-web.example.com",
		"_srv._tcp.example": "edns-srv.-tcp.example-",
	}
	for in, want := range cases {
		got := RecordSetName(in)
		if !strings.HasPrefix(got, want) {
			t.Errorf("RecordSetName(%q) = %q, want prefix %q", in, got, want)
		}
		if len(got) > 253 {
			t.Errorf("RecordSetName(%q) too long", in)
		}
	}
	long := strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." + strings.Repeat("c", 60) + "." + strings.Repeat("d", 60) + ".example.com"
	if n := RecordSetName(long); len(n) > 253 || !strings.HasPrefix(n, "edns-aaa") {
		t.Errorf("long name %q (%d)", n, len(n))
	}
	if RecordSetName("*.a.example") == RecordSetName("wildcard.a.example") {
		t.Error("wildcard and literal names collide")
	}
}

func TestRunServes(t *testing.T) {
	s, _ := newTestServer(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Serve(ctx, ln) }()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ln.Addr().String()+"/", nil)
	req.Header.Set("Accept", MediaType)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}

func TestOptionsFrom(t *testing.T) {
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	o := OptionsFrom(cfg.ExternalDNS.Webhook)
	if o.Namespace != "external-dns" || o.Address != ":8888" {
		t.Fatalf("options %+v", o)
	}
}
