package importers

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

func validatePlan(t *testing.T, p *Plan) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range p.Objects {
		u := &unstructured.Unstructured{Object: obj}
		if err := v.ValidateUnstructured(context.Background(), u, nil); err != nil {
			t.Errorf("%s/%s: %v", u.GetKind(), u.GetName(), err)
		}
	}
}

func TestPiHolePreservesGroupsListsAndLocalDNS(t *testing.T) {
	loggedOut := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/proxy/api/auth" {
			if r.Method == http.MethodDelete {
				loggedOut = true
				w.WriteHeader(204)
				return
			}
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["password"] != "app-secret" {
				t.Error("wrong app password")
			}
			_, _ = fmt.Fprint(w, `{"session":{"valid":true,"sid":"temporary-session"}}`)
			return
		}
		if r.Method != "GET" || r.Header.Get("X-FTL-SID") != "temporary-session" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		body, ok := map[string]string{
			"/groups":     `{"groups":[{"id":0,"name":"Default","enabled":true},{"id":1,"name":"Kids","enabled":true},{"id":2,"name":"Unfiltered","enabled":false}]}`,
			"/lists":      `{"lists":[{"id":4,"address":"https://example.net/hosts","type":"block","groups":[0,1],"enabled":false},{"id":5,"address":"https://example.net/allow","type":"allow","groups":[1],"enabled":true}]}`,
			"/domains":    `{"domains":[{"domain":"ads.example","type":"deny","kind":"exact","enabled":false,"groups":[0,1],"comment":"keep disabled"},{"domain":"needed.example","type":"allow","kind":"exact","enabled":true,"groups":[1]},{"domain":"ads;querytype=A","type":"deny","kind":"regex","enabled":true,"groups":[0]},{"domain":"(?=bad)","type":"deny","kind":"regex","enabled":true,"groups":[0]}]}`,
			"/clients":    `{"clients":[{"client":"192.0.2.5","groups":[1],"comment":"tablet"},{"client":"2001:db8::/64","groups":[1]},{"client":"192.0.2.6","groups":[2]},{"client":"aa:bb:cc:dd:ee:ff","groups":[1]}]}`,
			"/config/dns": `{"config":{"dns":{"upstreams":["192.0.2.53#5353"],"hosts":["192.0.2.10 nas.home.arpa","2001:db8::10 nas.home.arpa"],"cnameRecords":["files.home.arpa,nas.home.arpa,60"],"domain":{"name":"home.arpa"},"blockTTL":2,"blocking":{"active":true,"mode":"NX"}}}}`,
		}[strings.TrimPrefix(r.URL.Path, "/proxy/api")]
		if !ok {
			t.Errorf("unexpected endpoint %s", r.URL)
			w.WriteHeader(404)
			return
		}
		_, _ = fmt.Fprint(w, body)
	}))
	defer srv.Close()
	p, err := Preview(context.Background(), MigrationRequest{Source: "pihole", URL: srv.URL + "/proxy/admin/", Credential: "app-secret", Namespace: "dns"})
	if err != nil {
		t.Fatal(err)
	}
	if !loggedOut {
		t.Error("temporary session was not closed")
	}
	validatePlan(t, p)
	raw, _ := json.Marshal(p)
	text := string(raw)
	for _, want := range []string{`"comment":"keep disabled"`, `"enabled":false`, `"origin":"migrated"`, `"blockMode":"nxdomain"`, `192.0.2.53:5353`, `"recordType":"CNAME"`, `"recordTTL":60`, `2001:db8::10`, `"namespace":"dns"`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, secret := range []string{"app-secret", "temporary-session"} {
		if strings.Contains(text, secret) {
			t.Error("preview exposed source credentials")
		}
	}
	for _, obj := range p.Objects {
		if obj["kind"] == "Policy" && obj["metadata"].(map[string]any)["name"] == "pihole-group-2" {
			spec := obj["spec"].(map[string]any)
			if spec["blockListRefs"] != nil {
				t.Error("disabled group unexpectedly filters clients")
			}
		}
	}
	if !strings.Contains(text, "Skipped regex") || !strings.Contains(text, "Skipped client") {
		t.Error("missing unsupported-conversion warnings")
	}
}

func piHoleSource(t *testing.T, dnsConfig string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/auth":
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			_, _ = fmt.Fprint(w, `{"session":{"valid":true,"sid":"test-session"}}`)
		case "/api/groups":
			_, _ = fmt.Fprint(w, `{"groups":[{"id":0,"name":"Default","enabled":true}]}`)
		case "/api/lists":
			_, _ = fmt.Fprint(w, `{"lists":[]}`)
		case "/api/domains":
			_, _ = fmt.Fprint(w, `{"domains":[]}`)
		case "/api/clients":
			_, _ = fmt.Fprint(w, `{"clients":[]}`)
		case "/api/config/dns":
			_, _ = fmt.Fprintf(w, `{"config":{"dns":%s}}`, dnsConfig)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestPiHoleBlockingModes(t *testing.T) {
	for _, tc := range []struct {
		mode, want string
		warn       bool
	}{
		{"NX", "nxdomain", false},
		{"NULL", "zeroIP", false},
		{"IP", "zeroIP", true},
		{"IP_NODATA_AAAA", "zeroIP", true},
		{"NODATA", "zeroIP", true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			srv := piHoleSource(t, fmt.Sprintf(`{"blockTTL":2,"blocking":{"active":true,"mode":%q}}`, tc.mode))
			p, err := Preview(context.Background(), MigrationRequest{Source: "pihole", URL: srv.URL, Credential: "test-password"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, obj := range p.Objects {
				if obj["kind"] == "Policy" {
					found = true
					if mode := obj["spec"].(map[string]any)["blockMode"]; mode != tc.want {
						t.Errorf("blockMode=%v, want %s", mode, tc.want)
					}
				}
			}
			if !found {
				t.Fatal("missing imported policy")
			}
			warn := slices.ContainsFunc(p.Warnings, func(s string) bool { return strings.Contains(s, "blocking response needs manual review") })
			if warn != tc.warn {
				t.Errorf("blocking warning=%v, want %v", warn, tc.warn)
			}
		})
	}
}

func TestPiHoleCNAMETTL(t *testing.T) {
	for _, ttl := range []string{"60", " 60 ", "60abc", "60 extra", "60.5", "-1", "", " ", "9999999999999999999999999999"} {
		t.Run(ttl, func(t *testing.T) {
			cfg := fmt.Sprintf(`{"blocking":{"active":true,"mode":"NULL"},"domain":{"name":"home.arpa"},"cnameRecords":[%q]}`, "files.home.arpa,nas.home.arpa,"+ttl)
			srv := piHoleSource(t, cfg)
			p, err := Preview(context.Background(), MigrationRequest{Source: "pihole", URL: srv.URL, Credential: "test-password"})
			if strings.TrimSpace(ttl) != "60" {
				if err == nil || !strings.Contains(err.Error(), "invalid Pi-hole CNAME TTL") {
					t.Fatalf("invalid TTL accepted: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(p.Objects)
			if !strings.Contains(string(b), `"recordTTL":60`) || !strings.Contains(string(b), `"recordType":"CNAME"`) {
				t.Fatal("CNAME TTL was not preserved")
			}
		})
	}
}

func TestTechnitiumExportsAndErrors(t *testing.T) {
	failExport := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" || r.Header.Get("Authorization") != "Bearer source-secret" || r.URL.Query().Get("token") != "" {
			t.Errorf("unsafe source authentication: %s", r.URL.Path)
		}
		switch r.URL.Path {
		case "/api/zones/list":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"zones":[{"name":"home.arpa","type":"Primary"},{"name":"secondary.example","type":"Secondary"},{"name":"disabled.example","type":"Primary","disabled":true}]}}`)
		case "/api/settings/get":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"enableBlocking":true,"blockingType":"NxDomain","blockingAnswerTtl":30,"blockListUrls":["https://example.net/hosts","!https://example.net/allow","https://example.net/more-hosts","!https://example.net/more-allow","  # Allow Lists Below  ","","  ","  ! https://example.net/last-allow  "],"forwarders":["192.0.2.53"],"forwarderProtocol":"Udp"}}`)
		case "/api/allowed/export":
			_, _ = fmt.Fprintln(w, "needed.tracker.example")
		case "/api/blocked/export":
			if failExport {
				_, _ = fmt.Fprint(w, `{"status":"invalid-token","errorMessage":"source-secret"}`)
			} else {
				_, _ = fmt.Fprintln(w, "tracker.example")
			}
		case "/api/zones/records/get":
			if r.URL.Query().Get("listZone") != "true" {
				t.Error("must inspect all zone records")
			}
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"records":[{"name":"nas.home.arpa","type":"A","disabled":false}]}}`)
		case "/api/zones/options/get":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"queryAccess":"UseSpecifiedNetworkACL","queryAccessNetworkACL":["192.0.2.0/24"]}}`)
		case "/api/zones/export":
			_, _ = fmt.Fprint(w, "$ORIGIN home.arpa.\n@ 300 IN SOA ns.home.arpa. hostmaster.home.arpa. 10 3600 600 86400 300\n@ 300 IN NS ns.home.arpa.\nnas 120 IN A 192.0.2.10\n")
		default:
			t.Errorf("unexpected source path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	req := MigrationRequest{Source: "technitium", URL: srv.URL, Credential: "source-secret", Namespace: "dns"}
	p, err := Preview(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	validatePlan(t, p)
	b, _ := json.Marshal(p)
	text := string(b)
	for _, want := range []string{`*.tracker.example`, `*.needed.tracker.example`, `useNetworkACL`, `192.0.2.0/24`, `"recordTTL":120`, `Skipped Secondary zone`, `Skipped disabled zone`} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %s", want)
		}
	}
	subscriptions := map[string]map[string]string{}
	for _, obj := range p.Objects {
		if obj["kind"] != "BlockList" {
			continue
		}
		spec := obj["spec"].(map[string]any)
		mode := spec["mode"].(string)
		subscriptions[mode] = map[string]string{}
		for _, source := range spec["sources"].([]any) {
			s := source.(map[string]any)
			if u, ok := s["url"].(string); ok {
				subscriptions[mode][s["name"].(string)] = u
			}
		}
	}
	wantSubscriptions := map[string]map[string]string{
		"block": {"subscription-1": "https://example.net/hosts", "subscription-3": "https://example.net/more-hosts"},
		"allow": {"subscription-2": "https://example.net/allow", "subscription-4": "https://example.net/more-allow", "subscription-8": "https://example.net/last-allow"},
	}
	if !reflect.DeepEqual(subscriptions, wantSubscriptions) {
		t.Errorf("subscriptions=%v, want %v", subscriptions, wantSubscriptions)
	}
	failExport = true
	if _, err = Preview(context.Background(), req); err == nil || strings.Contains(err.Error(), "source-secret") {
		t.Fatalf("unsafe export error: %v", err)
	}
}

func TestSourceURLAndRedirects(t *testing.T) {
	for _, u := range []string{"file:///etc/passwd", "http://user:secret@example.net", "http://example.net?token=secret", "http://example.net/#secret"} {
		if _, err := Preview(context.Background(), MigrationRequest{Source: "technitium", URL: u, Credential: "secret"}); err == nil || strings.Contains(err.Error(), "secret") {
			t.Errorf("unsafe URL result for %s: %v", u, err)
		}
	}
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) { called = true }))
	defer target.Close()
	src := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, target.URL, http.StatusFound) }))
	defer src.Close()
	if _, err := Preview(context.Background(), MigrationRequest{Source: "technitium", URL: src.URL, Credential: "secret"}); err == nil {
		t.Error("redirect accepted")
	}
	if called {
		t.Error("source credentials could follow a redirect")
	}
}

func TestTechnitiumRejectsMissingResponseAndSkipsProxyForwarders(t *testing.T) {
	for _, response := range []string{`{"status":"ok"}`, `{"status":"ok","response":null}`} {
		t.Run(response, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = fmt.Fprint(w, response)
			}))
			defer srv.Close()
			if _, err := Preview(context.Background(), MigrationRequest{Source: "technitium", URL: srv.URL, Credential: "secret"}); err == nil {
				t.Fatal("missing API response accepted")
			}
		})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/zones/list":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"zones":[]}}`)
		case "/api/settings/get":
			_, _ = fmt.Fprint(w, `{"status":"ok","response":{"enableBlocking":true,"blockingType":"NxDomain","blockingAnswerTtl":30,"forwarders":["192.0.2.53"],"forwarderProtocol":"Udp","proxy":{"type":"Socks5","password":"proxy-secret"}}}`)
		case "/api/blocked/export", "/api/allowed/export":
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected endpoint %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	p, err := Preview(context.Background(), MigrationRequest{Source: "technitium", URL: srv.URL, Credential: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Settings) != 0 {
		t.Fatal("proxied forwarders were imported without their proxy")
	}
	b, _ := json.Marshal(p)
	if !strings.Contains(string(b), "outbound proxy") || strings.Contains(string(b), "proxy-secret") {
		t.Fatal("proxy warning missing or contained credentials")
	}
}
