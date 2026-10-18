package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api/apitest"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/memory"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/version"
)

var sharedValidator = sync.OnceValues(validator.New)

func testValidator(t testing.TB) *validator.Validator {
	t.Helper()
	v, err := sharedValidator()
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func testConfig(t testing.TB) *config.Config {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Node.Name = "node-a"
	cfg.Node.DataDir = t.TempDir()
	return cfg
}

type harness struct {
	t     testing.TB
	srv   *Server
	st    *memory.Store
	audit *audit.Memory
	now   time.Time
}

// newHarness builds a server over a memory store with the test
// authenticator and the standard bindings (users viewer, auditor,
// operator, admin; zoneadmin on zone "example").
func newHarness(t testing.TB, mods ...func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, st: memory.New(), audit: &audit.Memory{}, now: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	opts := Options{
		Store: h.st, Validator: testValidator(t), Config: testConfig(t),
		Authenticate: apitest.Authenticate, Authorizer: apitest.Authorizer(apitest.StandardBindings()...), CSRF: apitest.CSRF,
		Audit: h.audit, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Version: version.Info{Version: "v0.1.0-test", Commit: "abc", GoVersion: "go1.27"}, NodeName: "node-a",
	}
	for _, m := range mods {
		m(&opts)
	}
	srv, err := New(opts)
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	t.Cleanup(srv.Close)
	return h
}

type response struct {
	*httptest.ResponseRecorder
	t testing.TB
}

// json decodes the body into v.
func (r response) json(v any) {
	r.t.Helper()
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		r.t.Fatalf("decode %s: %v", r.Body.String(), err)
	}
}

// m decodes the body as a map.
func (r response) m() map[string]any {
	r.t.Helper()
	var out map[string]any
	r.json(&out)
	return out
}

// errKey is the envelope error key.
func (r response) errKey() string {
	r.t.Helper()
	var e struct {
		Error struct {
			Key string `json:"key"`
		} `json:"error"`
	}
	_ = json.Unmarshal(r.Body.Bytes(), &e)
	return e.Error.Key
}

func (r response) want(code int) response {
	r.t.Helper()
	if r.Code != code {
		r.t.Fatalf("status %d, want %d: %s", r.Code, code, r.Body.String())
	}
	return r
}

// req builds a request; body may be nil, []byte, string or any JSON value.
func (h *harness) req(method, path, user string, body any, hdr ...string) *http.Request {
	h.t.Helper()
	var rd io.Reader
	ctype := ""
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			h.t.Fatal(err)
		}
		rd = bytes.NewReader(raw)
		ctype = "application/json"
	}
	r := httptest.NewRequest(method, path, rd)
	r.RemoteAddr = "192.0.2.10:40000"
	if ctype != "" {
		r.Header.Set("Content-Type", ctype)
	}
	if user != "" {
		r.Header.Set("Authorization", apitest.Bearer(user))
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		r.Header.Set(hdr[i], hdr[i+1])
	}
	return r
}

func (h *harness) serve(r *http.Request) response {
	rec := httptest.NewRecorder()
	h.srv.ServeHTTP(rec, r)
	return response{ResponseRecorder: rec, t: h.t}
}

func (h *harness) do(method, path, user string, body any, hdr ...string) response {
	h.t.Helper()
	return h.serve(h.req(method, path, user, body, hdr...))
}

// seed applies objects directly to the store with manager.
func (h *harness) seed(manager string, objs ...store.Object) {
	h.t.Helper()
	for _, o := range objs {
		if err := testValidator(h.t).Validate(context.Background(), o, nil); err != nil {
			h.t.Fatalf("seed %s: %v", store.KeyOf(o), err)
		}
		if _, err := h.st.Apply(context.Background(), o, store.ApplyOptions{Manager: manager}); err != nil {
			h.t.Fatalf("seed %s: %v", store.KeyOf(o), err)
		}
	}
}

func zoneObj(name, apex string) *v1alpha1.DNSZone {
	return &v1alpha1.DNSZone{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSZone"},
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: apex, Type: v1alpha1.ZoneTypePrimary, Nameservers: []string{"ns1." + apex}},
	}
}

func recordSetObj(ns, name, zone string, eps ...v1alpha1.Endpoint) *v1alpha1.DNSRecordSet {
	return &v1alpha1.DNSRecordSet{
		TypeMeta:   metav1.TypeMeta{APIVersion: v1alpha1.GroupVersion.String(), Kind: "DNSRecordSet"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec:       v1alpha1.DNSRecordSetSpec{ZoneRef: v1alpha1.ObjectRef{Name: zone}, Endpoints: eps},
	}
}

func ep(name, typ string, targets ...string) v1alpha1.Endpoint {
	return v1alpha1.Endpoint{DNSName: name, RecordType: typ, Targets: targets, RecordTTL: 300}
}

// zoneDoc is a DNSZone document as a client would send it.
func zoneDoc(name, apex string) map[string]any {
	return map[string]any{
		"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone",
		"metadata": map[string]any{"name": name},
		"spec":     map[string]any{"zoneName": apex, "type": "primary", "nameservers": []any{"ns1." + apex}},
	}
}

func objectMeta(ns, name string) metav1.ObjectMeta {
	return metav1.ObjectMeta{Namespace: ns, Name: name}
}

// staticLoader is an objects.Loader in the ha-static shape.
func staticLoader(t testing.TB, st store.Store) *objects.Loader {
	t.Helper()
	l, err := objects.New(objects.Options{Store: st, Validator: testValidator(t), Static: true, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return l
}

// loaderFor is a file-mode objects.Loader over dir.
func loaderFor(t testing.TB, st store.Store, dir string) *objects.Loader {
	t.Helper()
	l, err := objects.New(objects.Options{Store: st, Validator: testValidator(t), Dir: dir, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return l
}
