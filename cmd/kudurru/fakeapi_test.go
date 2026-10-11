package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI implements the subset of the /api/v1 contract the CLI uses, with
// the contract's envelopes, errors, auth (bearer token, session cookie +
// CSRF) and keyset pagination.
type fakeAPI struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	tokens   map[string]string // bearer token -> principal name
	users    map[string]string // username -> password
	sessions map[string]string // session cookie -> username
	objects  map[string]map[string]json.RawMessage
	// pageSize forces small pages to exercise "continue".
	pageSize int
	requests []string
	bodies   map[string][]byte // "METHOD path" -> last body
	headers  map[string]http.Header

	blocking map[string]any
	settings map[string]any
	nodeCfg  map[string]any
	records  []map[string]any
	queries  []map[string]any
	// stream holds SSE batches: each connection sends the next batch and
	// closes; the last batch is held open until the client goes away.
	stream     [][]string
	streamConn int
	lastIDs    []string
	messages   []map[string]any
}

const (
	fakeToken = "kdr_abc123def456_s3cr3tS3cr3tS3cr3tS3cr3tS3cr3tS3cr3tS3cr3tS3c"
	fakeCSRF  = "csrf-123"
)

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	f := &fakeAPI{
		t:        t,
		tokens:   map[string]string{fakeToken: "admin"},
		users:    map[string]string{"admin": "hunter2"},
		sessions: map[string]string{},
		objects:  map[string]map[string]json.RawMessage{},
		bodies:   map[string][]byte{},
		headers:  map[string]http.Header{},
		blocking: map[string]any{"blocking": true},
		settings: map[string]any{"cache": map[string]any{"maxEntries": 100000.0, "minTTL": "0s"}, "blocking": map[string]any{"enabled": true}},
		nodeCfg:  map[string]any{"log": map[string]any{"level": "info", "format": "json"}, "node": map[string]any{"name": "node-a", "dataDir": "/var/lib/kudurru"}},
	}
	f.seed()
	mux := http.NewServeMux()
	f.routes(mux)
	f.srv = httptest.NewServer(f.middleware(mux))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPI) URL() string { return f.srv.URL }

func (f *fakeAPI) seed() {
	add := func(kind, ns, name, js string) {
		if f.objects[kind] == nil {
			f.objects[kind] = map[string]json.RawMessage{}
		}
		f.objects[kind][ns+"/"+name] = json.RawMessage(js)
	}
	add("dnszones", "", "home-example-com", `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"home-example-com","creationTimestamp":"2026-10-01T12:00:00Z","resourceVersion":"41","generation":2,"uid":"u-1","annotations":{"kudurru.io/managed-by":"api"}},
		"spec":{"zoneName":"home.example.com.","type":"primary","nameservers":["ns1.home.example.com."],"defaultTTL":300},
		"status":{"serial":2026100101,"recordCount":12,"contentHash":"0123456789abcdef0123","conditions":[{"type":"Ready","status":"True","reason":"AsExpected","message":"zone built","lastTransitionTime":"2026-10-11T11:00:00Z","observedGeneration":2}]}}`)
	add("dnszones", "", "lab-example-com", `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSZone","metadata":{"name":"lab-example-com","creationTimestamp":"2026-10-11T09:00:00Z","annotations":{"kudurru.io/managed-by":"file:objects.d/zones.yaml"}},
		"spec":{"zoneName":"lab.example.com.","type":"forward"},
		"status":{"conditions":[{"type":"Ready","status":"False","reason":"NoUpstream","message":"no forwarders","lastTransitionTime":"2026-10-11T11:30:00Z"}]}}`)
	add("dnsrecordsets", "default", "web", `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSRecordSet","metadata":{"name":"web","namespace":"default","creationTimestamp":"2026-10-10T12:00:00Z","labels":{"team":"web"}},
		"spec":{"zoneRef":{"name":"home-example-com"},"endpoints":[{"dnsName":"www.home.example.com","recordType":"A","targets":["192.168.1.10"]},{"dnsName":"home.example.com","recordType":"MX","targets":["10 mail.home.example.com."]}]},
		"status":{"names":2,"conditions":[{"type":"Ready","status":"True","reason":"AsExpected","lastTransitionTime":"2026-10-10T12:00:00Z"}]}}`)
	add("dnsrecordsets", "apps", "api", `{"apiVersion":"kudurru.io/v1alpha1","kind":"DNSRecordSet","metadata":{"name":"api","namespace":"apps","creationTimestamp":"2026-10-11T11:55:00Z","labels":{"team":"platform"},"annotations":{"kudurru.io/managed-by":"integration:external-dns"}},
		"spec":{"zoneRef":{"name":"home-example-com"},"endpoints":[{"dnsName":"api.home.example.com","recordType":"CNAME","targets":["lb.home.example.com"]}]},
		"status":{"names":1}}`)
	add("blocklists", "default", "ads", `{"apiVersion":"kudurru.io/v1alpha1","kind":"BlockList","metadata":{"name":"ads","namespace":"default","creationTimestamp":"2026-09-11T12:00:00Z"},
		"spec":{"mode":"block","sources":[{"name":"stevenblack","url":"https://lists.example/hosts?token=abc123"},{"name":"local","inline":["ads.example.net"]}],"refreshInterval":"24h"},
		"status":{"entries":81234,"ruleCount":81240,"invalid":6,"lastFetched":"2026-10-11T10:00:00Z","conditions":[{"type":"Ready","status":"True","reason":"Fetched","lastTransitionTime":"2026-10-11T10:00:00Z"}]}}`)
	add("blocklists", "default", "allow", `{"apiVersion":"kudurru.io/v1alpha1","kind":"BlockList","metadata":{"name":"allow","namespace":"default","creationTimestamp":"2026-09-11T12:00:00Z"},
		"spec":{"mode":"allow","sources":[{"name":"inline","inline":["good.example.com"]}]},
		"status":{"entries":1,"conditions":[{"type":"Ready","status":"True"}]}}`)
	add("clientgroups", "default", "kids", `{"apiVersion":"kudurru.io/v1alpha1","kind":"ClientGroup","metadata":{"name":"kids","namespace":"default","creationTimestamp":"2026-10-04T12:00:00Z"},
		"spec":{"clients":[{"cidr":"192.168.1.64/28"},{"ip":"192.168.1.23"}]},"status":{"members":3,"conditions":[{"type":"Ready","status":"True"}]}}`)
	add("policies", "default", "default", `{"apiVersion":"kudurru.io/v1alpha1","kind":"Policy","metadata":{"name":"default","namespace":"default","creationTimestamp":"2026-09-01T12:00:00Z","annotations":{"kudurru.io/managed-by":"bootstrap"}},
		"spec":{"blockListRefs":["ads"],"allowListRefs":["allow"],"priority":0},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)
	add("policies", "default", "kids", `{"apiVersion":"kudurru.io/v1alpha1","kind":"Policy","metadata":{"name":"kids","namespace":"default","creationTimestamp":"2026-10-04T12:00:00Z"},
		"spec":{"clientGroupRefs":["kids"],"blockListRefs":["ads"],"priority":10,"upstreamGroup":"family"},"status":{"conditions":[{"type":"Ready","status":"True"}]}}`)

	f.records = []map[string]any{
		{"recordSet": "web", "namespace": "default", "dnsName": "www.home.example.com", "recordType": "A", "targets": []string{"192.168.1.10"}, "recordTTL": 300},
		{"recordSet": "web", "namespace": "default", "dnsName": "home.example.com", "recordType": "MX", "targets": []string{"10 mail.home.example.com."}},
		{"recordSet": "api-home-example-com", "dnsName": "nas.home.example.com", "recordType": "A", "targets": []string{"192.168.1.20", "192.168.1.21"}, "recordTTL": 60, "kudurru": map[string]any{"comment": "the NAS", "disabled": true}},
	}
	base := time.Date(2026, 10, 11, 11, 59, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		row := map[string]any{
			"id": i, "ts": base.Add(time.Duration(i) * time.Second).Format(time.RFC3339Nano), "node": "node-a", "proto": "udp",
			"client": map[string]any{"ip": "192.168.1." + strconv.Itoa(20+i)}, "domain": fmt.Sprintf("host%d.example.com", i), "qtype": "A", "qclass": 1,
			"status": "forwarded", "status_class": "permitted", "rcode": "NOERROR", "reply": map[string]any{"type": "ip", "time_ms": 1.5 * float64(i), "answer": "93.184.216.34"},
			"upstream": "default", "dnssec": "unknown", "flags": map[string]any{},
		}
		if i == 4 {
			row["status"], row["status_class"], row["list"], row["rule"], row["upstream"] = "blocked_list", "blocked", "ads", "||host4.example.com^", nil
			row["client"] = map[string]any{"ip": "192.168.1.24", "name": "laptop"}
		}
		f.queries = append(f.queries, row)
	}
	// Newest first, like the query log.
	slices.Reverse(f.queries)
	// internal/api DiagnosisMessage: object is the store key.
	f.messages = []map[string]any{
		{"severity": "error", "source": "objects", "object": "DNSZone/lab-example-com", "reason": "NoUpstream", "message": "forward zone has no forwarders", "hint": "set spec.forward.servers", "node": "node-a"},
		{"severity": "warning", "source": "policy", "object": "Policy/default/kids", "reason": "PriorityTie", "message": "same priority as policy other", "node": "node-a"},
		{"severity": "info", "source": "cluster", "reason": "ReadOnly", "message": "objects are read-only through the API (ha-static)", "node": "node-a"},
	}
}

func (f *fakeAPI) writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeAPI) writeErr(w http.ResponseWriter, status int, key, msg string, hint any) {
	f.writeJSON(w, status, map[string]any{"error": map[string]any{"key": key, "message": msg, "hint": hint}, "took_ms": 0.1})
}

// middleware authenticates every route except the public ones and
// enforces CSRF on cookie-authenticated writes.
func (f *fakeAPI) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		f.mu.Lock()
		key := r.Method + " " + r.URL.Path
		f.requests = append(f.requests, r.Method+" "+r.URL.RequestURI())
		f.bodies[key] = body
		f.headers[key] = r.Header.Clone()
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/version", "/api/v1/auth/login", "/api/v1/auth/status":
			next.ServeHTTP(w, r)
			return
		}
		if status, key, msg := f.authCheck(r); status != 0 {
			f.writeErr(w, status, key, msg, "")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authCheck returns a non-zero status when the request is not admitted.
func (f *fakeAPI) authCheck(r *http.Request) (int, string, string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := r.Header.Get("Authorization"); h != "" {
		if _, ok := f.tokens[strings.TrimPrefix(h, "Bearer ")]; !ok {
			return 401, "unauthenticated", "invalid token"
		}
		return 0, "", ""
	}
	if c, err := r.Cookie("kudurru_session"); err == nil && f.sessions[c.Value] != "" {
		if r.Method != http.MethodGet && r.Header.Get("X-Kudurru-CSRF") != fakeCSRF {
			return 403, "forbidden", "missing CSRF header"
		}
		return 0, "", ""
	}
	return 401, "unauthenticated", "no credentials"
}

func (f *fakeAPI) lastBody(method, path string) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.bodies[method+" "+path]
}

func (f *fakeAPI) lastHeader(method, path string) http.Header {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.headers[method+" "+path]
}

func (f *fakeAPI) requested(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, r := range f.requests {
		if strings.HasPrefix(r, prefix) {
			out = append(out, r)
		}
	}
	return out
}

func (f *fakeAPI) principal(r *http.Request) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	if h := r.Header.Get("Authorization"); h != "" {
		return f.tokens[strings.TrimPrefix(h, "Bearer ")]
	}
	if c, err := r.Cookie("kudurru_session"); err == nil {
		return f.sessions[c.Value]
	}
	return ""
}

func (f *fakeAPI) routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/version", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 200, map[string]any{"version": "v0.1.0-test", "took_ms": 0.1})
	})
	mux.HandleFunc("POST /api/v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Username, Password string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		ok := f.users[req.Username] == req.Password && req.Password != ""
		if ok {
			f.sessions["sess-1"] = req.Username
		}
		f.mu.Unlock()
		if !ok {
			f.writeErr(w, 401, "unauthenticated", "invalid username or password", "")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "kudurru_session", Value: "sess-1", Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode}) //nolint:gosec // G124: httptest serves plain HTTP, a Secure cookie would never come back
		f.writeJSON(w, 200, map[string]any{"took_ms": 1})
	})
	mux.HandleFunc("POST /api/v1/auth/logout", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		delete(f.sessions, "sess-1")
		f.mu.Unlock()
		w.WriteHeader(204)
	})
	mux.HandleFunc("GET /api/v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		name := f.principal(r)
		me := map[string]any{"principal": map[string]any{"kind": "User", "id": "u-" + name, "name": name}, "roles": []string{"admin"},
			"permissions": map[string][]string{"zones": {"read", "write"}}}
		if _, err := r.Cookie("kudurru_session"); err == nil && r.Header.Get("Authorization") == "" {
			me["csrf"] = fakeCSRF
		}
		f.writeJSON(w, 200, me)
	})
	mux.HandleFunc("POST /api/v1/auth/tokens", func(w http.ResponseWriter, r *http.Request) {
		if _, err := r.Cookie("kudurru_session"); err != nil {
			f.writeErr(w, 400, "invalid", "test expects a session here", "")
			return
		}
		var req struct{ Description string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		tok := "kdr_minted000001_" + strings.Repeat("m", 43)
		f.mu.Lock()
		f.tokens[tok] = f.sessions["sess-1"]
		f.mu.Unlock()
		f.writeJSON(w, 201, map[string]any{"id": "minted000001", "description": req.Description, "token": tok})
	})

	mux.HandleFunc("GET /api/v1/objects/{kind}", f.listObjects)
	mux.HandleFunc("GET /api/v1/objects/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) { f.getObject(w, r, "", r.PathValue("name")) })
	mux.HandleFunc("GET /api/v1/objects/{kind}/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.getObject(w, r, r.PathValue("ns"), r.PathValue("name"))
	})
	mux.HandleFunc("DELETE /api/v1/objects/{kind}/{name}", func(w http.ResponseWriter, r *http.Request) { f.deleteObject(w, r, "", r.PathValue("name")) })
	mux.HandleFunc("DELETE /api/v1/objects/{kind}/{ns}/{name}", func(w http.ResponseWriter, r *http.Request) {
		f.deleteObject(w, r, r.PathValue("ns"), r.PathValue("name"))
	})
	mux.HandleFunc("POST /api/v1/apply", f.apply)

	mux.HandleFunc("GET /api/v1/zones", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		var items []map[string]any
		for _, key := range sortedKeys(f.objects["dnszones"]) {
			var z struct {
				Metadata struct {
					Name        string            `json:"name"`
					Annotations map[string]string `json:"annotations"`
				} `json:"metadata"`
				Spec struct {
					ZoneName string `json:"zoneName"`
					Type     string `json:"type"`
				} `json:"spec"`
				Status struct {
					Serial      uint32 `json:"serial"`
					RecordCount int    `json:"recordCount"`
					Conditions  []struct{ Type, Status string }
				} `json:"status"`
			}
			_ = json.Unmarshal(f.objects["dnszones"][key], &z)
			ready := false
			for _, c := range z.Status.Conditions {
				ready = ready || (c.Type == "Ready" && c.Status == "True")
			}
			items = append(items, map[string]any{"name": z.Metadata.Name, "zoneName": z.Spec.ZoneName, "type": z.Spec.Type, "serial": z.Status.Serial,
				"recordCount": z.Status.RecordCount, "ready": ready, "managedBy": z.Metadata.Annotations["kudurru.io/managed-by"]})
		}
		f.mu.Unlock()
		f.writeJSON(w, 200, map[string]any{"items": items, "took_ms": 0.2})
	})
	mux.HandleFunc("GET /api/v1/zones/{zone}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("zone") != "home-example-com" {
			f.writeErr(w, 404, "not_found", "zone "+r.PathValue("zone")+" not found", "")
			return
		}
		f.writeJSON(w, 200, map[string]any{"name": "home-example-com", "zoneName": "home.example.com.", "type": "primary", "serial": 2026100101,
			"recordCount": 12, "ready": "True", "managedBy": "api", "took_ms": 0.1})
	})
	mux.HandleFunc("GET /api/v1/zones/{zone}/records", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 200, map[string]any{"items": f.records})
	})
	mux.HandleFunc("POST /api/v1/zones/{zone}/records", func(w http.ResponseWriter, r *http.Request) {
		var ep map[string]any
		if err := json.NewDecoder(r.Body).Decode(&ep); err != nil || ep["dnsName"] == nil {
			f.writeErr(w, 400, "invalid", "dnsName: required", "")
			return
		}
		f.writeJSON(w, 200, map[string]any{"recordSet": firstNonEmpty(r.URL.Query().Get("recordSet"), "api-"+r.PathValue("zone")), "result": "updated"})
	})
	mux.HandleFunc("DELETE /api/v1/zones/{zone}/records/{name}/{type}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") == "missing.home.example.com" {
			f.writeErr(w, 404, "not_found", "no record missing.home.example.com A", "")
			return
		}
		w.WriteHeader(204)
	})
	mux.HandleFunc("POST /api/v1/zones/{zone}/import", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "text/dns" {
			f.writeErr(w, 415, "invalid", "want text/dns", "")
			return
		}
		f.writeJSON(w, 200, map[string]any{"zone": r.PathValue("zone"), "zoneName": "home.example.com.", "created": r.URL.Query().Get("origin") != "",
			"recordSets": []string{"default/import-home-example-com"}, "endpoints": 3, "serial": 2026101101, "warnings": []string{"line 9: $INCLUDE ignored"}, "took_ms": 3})
	})
	mux.HandleFunc("GET /api/v1/zones/{zone}/export", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/dns")
		_, _ = fmt.Fprintf(w, "$ORIGIN home.example.com.\n@\t300\tIN\tSOA\tns1 hostmaster 2026100101 3600 600 604800 300\nwww\t300\tIN\tA\t192.168.1.10\n")
	})
	mux.HandleFunc("POST /api/v1/zones/{zone}/notify", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 202, map[string]any{"targets": []string{"192.0.2.53"}})
	})

	mux.HandleFunc("GET /api/v1/blocklists", f.listObjectsKind("blocklists"))
	mux.HandleFunc("POST /api/v1/blocklists/{ns}/{name}/refresh", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("name") != "ads" {
			f.writeErr(w, 404, "not_found", "blocklist "+r.PathValue("name")+" not found", "")
			return
		}
		f.writeJSON(w, 202, map[string]any{})
	})
	mux.HandleFunc("GET /api/v1/blocklists/_search", func(w http.ResponseWriter, r *http.Request) {
		f.writeJSON(w, 200, map[string]any{"items": []map[string]any{
			{"list": "default/ads", "mode": "block", "rule": "||" + r.URL.Query().Get("domain") + "^"},
			{"list": "home/kids-extra", "mode": "block", "rule": "ads.example.net"},
		}})
	})
	mux.HandleFunc("POST /api/v1/domains/{verb}/{kind}", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Domains []string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		var ok []string
		var bad []map[string]string
		for _, d := range req.Domains {
			if strings.Contains(d, " ") {
				bad = append(bad, map[string]string{"item": d, "error": "invalid domain"})
			} else {
				ok = append(ok, d)
			}
		}
		f.writeJSON(w, 200, map[string]any{"processed": map[string]any{"success": ok, "errors": bad}})
	})

	mux.HandleFunc("GET /api/v1/dns/blocking", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.writeJSON(w, 200, f.blocking)
	})
	mux.HandleFunc("POST /api/v1/dns/blocking", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		_ = json.NewDecoder(r.Body).Decode(&req)
		st := map[string]any{"blocking": req["blocking"]}
		if t, ok := req["timer"].(float64); ok {
			st["timer"] = t
			st["until"] = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC).Add(time.Duration(t) * time.Second).Format(time.RFC3339)
		}
		f.mu.Lock()
		f.blocking = st
		f.mu.Unlock()
		f.writeJSON(w, 200, st)
	})
	mux.HandleFunc("POST /api/v1/cache/flush", func(w http.ResponseWriter, r *http.Request) {
		n := 4211
		if r.URL.Query().Get("name") != "" {
			n = 3
		}
		f.writeJSON(w, 200, map[string]any{"flushed": n})
	})
	mux.HandleFunc("POST /api/v1/dns/query", func(w http.ResponseWriter, r *http.Request) {
		var req struct{ Name, Type, Client string }
		_ = json.NewDecoder(r.Body).Decode(&req)
		if strings.HasPrefix(req.Name, "ads.") {
			f.writeJSON(w, 200, map[string]any{"name": req.Name + ".", "type": req.Type, "client": req.Client, "status": "blocked_list", "rcode": 0,
				"list": "ads", "rule": "||ads.example.net^", "node": "node-a", "latency_ms": 0.2,
				"answer": []map[string]any{{"name": req.Name, "ttl": 600, "class": "IN", "type": "A", "data": "0.0.0.0"}}})
			return
		}
		f.writeJSON(w, 200, map[string]any{"name": req.Name + ".", "type": req.Type, "client": "127.0.0.1", "status": "forwarded", "status_class": "permitted", "rcode": "NOERROR",
			"upstream": "default", "node": "node-a", "latency_ms": 12.5, "flags": map[string]bool{"rd": true, "ra": true},
			"answer":    []string{req.Name + ". 300 IN CNAME edge.example.net.", "edge.example.net. 60 IN A 93.184.216.34"},
			"authority": []string{}, "additional": []string{}})
	})

	mux.HandleFunc("GET /api/v1/stats/summary", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 200, map[string]any{"scope": "node", "nodes": []map[string]any{{"name": "node-a", "ok": true, "took_ms": 1.2}}, "partial": false,
			"from": "2026-10-10T12:00:00Z", "until": "2026-10-11T12:00:00Z",
			"queries":          map[string]any{"total": 120000, "blocked": 18000, "percent_blocked": 15, "cached": 50000, "forwarded": 48000, "authoritative": 4000, "dropped": 0, "other": 0, "unique_domains": 3456, "qps_1m": 2.5, "qps_avg": 1.4},
			"clients":          map[string]any{"active": 14, "total": 20},
			"domains_on_lists": 81234,
			"latency":          map[string]any{"avg_ms": 4.2, "p50_ms": 0.3, "p95_ms": 21, "p99_ms": 80},
			"approximate":      false})
	})
	mux.HandleFunc("GET /api/v1/stats/top", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("dim") == "upstreams" {
			f.writeJSON(w, 200, map[string]any{"scope": "node", "nodes": []map[string]any{{"name": "node-a", "ok": true}}, "dim": "upstreams", "items": []any{},
				"upstreams": []map[string]any{{"addr": "default", "total": 48000, "errors": 12, "timeouts": 3, "percent": 92.3, "avg_ms": 14.2, "p95_ms": 40}, {"addr": "cache", "total": 4000, "percent": 7.7}}})
			return
		}
		f.writeJSON(w, 200, map[string]any{"scope": map[string]any{"scope": "node", "nodes": []map[string]any{{"name": "node-a", "ok": true}}}, "dim": r.URL.Query().Get("dim"),
			"blocked": r.URL.Query().Get("blocked") == "true", "items": []map[string]any{
				{"key": "ads.example.net", "count": 900, "blocked": 900, "percent": 5},
				{"key": "tracker.example.org", "count": 450, "blocked": 450, "percent": 2.5},
			}})
	})
	mux.HandleFunc("GET /api/v1/queries", f.queriesPage)
	mux.HandleFunc("GET /api/v1/queries/stream", f.queriesStream)
	mux.HandleFunc("GET /api/v1/nodes", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 200, map[string]any{"items": []map[string]any{
			{"name": "node-a", "address": "10.0.0.11", "self": true, "leader": true, "ready": true, "version": "v0.1.0", "state": "live"},
			{"name": "node-b", "address": "10.0.0.12", "self": false, "leader": false, "ready": false, "unready": []string{"zonecache"}, "version": "v0.1.0", "state": "fromSnapshot", "snapshotAge": "3m"},
		}, "took_ms": 1})
	})
	mux.HandleFunc("GET /api/v1/audit", func(w http.ResponseWriter, r *http.Request) {
		// internal/audit.Event rows.
		items := []map[string]any{
			{"id": 2, "ts": "2026-10-11T11:00:00Z", "node": "node-a", "actor": map[string]any{"kind": "User", "id": "u-admin", "name": "admin"}, "action": "update",
				"resource": map[string]any{"kind": "DNSZone", "name": "home-example-com"}, "result": "success", "authz": "binding"},
			{"id": 1, "ts": "2026-10-11T10:00:00Z", "node": "node-a", "actor": map[string]any{"kind": "ServiceAccount", "id": "sa-ci", "name": "ci"}, "action": "delete",
				"resource": map[string]any{"kind": "BlockList", "namespace": "default", "name": "old"}, "result": "denied", "reason": "forbidden"},
		}
		if r.URL.Query().Get("cursor") == "" {
			f.writeJSON(w, 200, map[string]any{"items": items[:1], "cursor_next": "c1"})
			return
		}
		f.writeJSON(w, 200, map[string]any{"items": items[1:]})
	})
	mux.HandleFunc("GET /api/v1/info/messages", func(w http.ResponseWriter, _ *http.Request) {
		f.writeJSON(w, 200, map[string]any{"items": f.messages})
	})
	// internal/api SettingsBody: {scope, node, settings:{tree}}.
	mux.HandleFunc("GET /api/v1/settings", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.writeJSON(w, 200, map[string]any{"scope": "cluster", "node": "node-a", "settings": f.settings, "took_ms": 0.2})
	})
	mux.HandleFunc("GET /api/v1/node/config", func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		f.writeJSON(w, 200, map[string]any{"scope": "node", "node": "node-a", "settings": f.nodeCfg, "took_ms": 0.2})
	})
	patch := func(w http.ResponseWriter, r *http.Request) {
		if ct := r.Header.Get("Content-Type"); ct != "application/merge-patch+json" {
			f.writeErr(w, 415, "invalid", "want merge patch, got "+ct, "")
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		var changed, restart []string
		var walk func(prefix string, m map[string]any)
		walk = func(prefix string, m map[string]any) {
			for _, k := range sortedKeys(m) {
				if sub, ok := m[k].(map[string]any); ok {
					walk(prefix+k+".", sub)
					continue
				}
				changed = append(changed, prefix+k)
				if prefix+k == "log.format" {
					restart = append(restart, prefix+k)
				}
			}
		}
		walk("", body)
		f.writeJSON(w, 200, map[string]any{"scope": "cluster", "node": "node-a", "settings": map[string]any{}, "changed": changed, "restartRequired": restart})
	}
	mux.HandleFunc("PATCH /api/v1/settings", patch)
	mux.HandleFunc("PATCH /api/v1/node/config", patch)
}

// listObjectsKind serves a kind's list for routes outside /objects.
func (f *fakeAPI) listObjectsKind(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.SetPathValue("kind", kind)
		f.listObjects(w, r)
	}
}

func (f *fakeAPI) listObjects(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	q := r.URL.Query()
	f.mu.Lock()
	objs, ok := f.objects[kind]
	known := ok || slices.Contains([]string{"dnszones", "dnsrecordsets", "blocklists", "clientgroups", "policies"}, kind)
	keys := sortedKeys(objs)
	f.mu.Unlock()
	if !known {
		f.writeErr(w, 404, "not_found", "unknown kind "+kind, "")
		return
	}
	var items []json.RawMessage
	for _, k := range keys {
		ns, _, _ := strings.Cut(k, "/")
		if want := q.Get("namespace"); want != "" && ns != want {
			continue
		}
		if sel := q.Get("labelSelector"); sel != "" {
			var m struct {
				Metadata struct{ Labels map[string]string } `json:"metadata"`
			}
			_ = json.Unmarshal(objs[k], &m)
			lk, lv, _ := strings.Cut(sel, "=")
			if m.Metadata.Labels[lk] != lv {
				continue
			}
		}
		items = append(items, objs[k])
	}
	start, _ := strconv.Atoi(q.Get("continue"))
	size := len(items)
	if f.pageSize > 0 {
		size = f.pageSize
	}
	if l, _ := strconv.Atoi(q.Get("limit")); l > 0 && l < size {
		size = l
	}
	end := min(start+size, len(items))
	page := map[string]any{"items": items[min(start, len(items)):end], "took_ms": 0.3}
	if items == nil {
		page["items"] = []any{}
	}
	if end < len(items) {
		page["continue"] = strconv.Itoa(end)
	}
	f.writeJSON(w, 200, page)
}

func (f *fakeAPI) getObject(w http.ResponseWriter, r *http.Request, ns, name string) {
	f.mu.Lock()
	obj, ok := f.objects[r.PathValue("kind")][ns+"/"+name]
	f.mu.Unlock()
	if !ok {
		f.writeErr(w, 404, "not_found", fmt.Sprintf("%s %q not found", r.PathValue("kind"), name), "")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(obj)
}

func (f *fakeAPI) deleteObject(w http.ResponseWriter, r *http.Request, ns, name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	kind := r.PathValue("kind")
	obj, ok := f.objects[kind][ns+"/"+name]
	if !ok {
		f.writeErr(w, 404, "not_found", fmt.Sprintf("%s %q not found", kind, name), "")
		return
	}
	if strings.Contains(string(obj), `"kudurru.io/managed-by":"file:`) {
		f.writeErr(w, 409, "managed_by", "object is managed by a file", "file:objects.d/zones.yaml")
		return
	}
	delete(f.objects[kind], ns+"/"+name)
	w.WriteHeader(204)
}

func (f *fakeAPI) apply(w http.ResponseWriter, r *http.Request) {
	var docs []map[string]any
	if err := json.NewDecoder(r.Body).Decode(&docs); err != nil {
		f.writeErr(w, 400, "invalid", "body: "+err.Error(), "")
		return
	}
	plural := map[string]string{"DNSZone": "dnszones", "DNSRecordSet": "dnsrecordsets", "BlockList": "blocklists", "ClientGroup": "clientgroups", "Policy": "policies"}
	var results []map[string]any
	f.mu.Lock()
	for _, d := range docs {
		kind, _ := d["kind"].(string)
		meta, _ := d["metadata"].(map[string]any)
		name, _ := meta["name"].(string)
		ns, _ := meta["namespace"].(string)
		res := map[string]any{"kind": kind, "name": name}
		if ns != "" {
			res["namespace"] = ns
		}
		p, ok := plural[kind]
		existing, exists := f.objects[p][ns+"/"+name]
		switch {
		case !ok:
			res["result"], res["error"] = "error", "unknown kind "+kind
		case exists && strings.Contains(string(existing), `"kudurru.io/managed-by":"file:`):
			res["result"], res["error"], res["errorKey"], res["hint"] = "error", "object is managed by another manager", "managed_by", "file:objects.d/zones.yaml"
		default:
			b, _ := json.Marshal(d)
			switch {
			case !exists:
				res["result"] = "created"
			case jsonEqualSpec(existing, b):
				res["result"] = "unchanged"
			default:
				res["result"] = "updated"
			}
			if f.objects[p] == nil {
				f.objects[p] = map[string]json.RawMessage{}
			}
			if res["result"] != "unchanged" {
				f.objects[p][ns+"/"+name] = b
			}
		}
		results = append(results, res)
	}
	f.mu.Unlock()
	f.writeJSON(w, 200, map[string]any{"items": results, "took_ms": 2})
}

func jsonEqualSpec(a, b []byte) bool {
	var x, y struct {
		Spec json.RawMessage `json:"spec"`
	}
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	var xv, yv any
	_ = json.Unmarshal(x.Spec, &xv)
	_ = json.Unmarshal(y.Spec, &yv)
	xs, _ := json.Marshal(xv)
	ys, _ := json.Marshal(yv)
	return string(xs) == string(ys)
}

// queriesPage serves GET /queries with a decimal id cursor, two rows per
// page so the CLI must follow cursor_next.
func (f *fakeAPI) queriesPage(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	rows := slices.Clone(f.queries)
	f.mu.Unlock()
	var filtered []map[string]any
	for _, row := range rows {
		if c := q.Get("client"); c != "" && row["client"].(map[string]any)["ip"] != c {
			continue
		}
		if s := q.Get("status"); s != "" && !slices.Contains(strings.Split(s, ","), row["status"].(string)) {
			continue
		}
		filtered = append(filtered, row)
	}
	cursor, _ := strconv.Atoi(q.Get("cursor"))
	var page []map[string]any
	limit, _ := strconv.Atoi(q.Get("limit"))
	limit = min(max(limit, 1), 2)
	for _, row := range filtered {
		if id := row["id"].(int); cursor == 0 || id < cursor {
			page = append(page, row)
			if len(page) == limit {
				break
			}
		}
	}
	resp := map[string]any{"scope": "node", "nodes": []map[string]any{{"name": "node-a", "ok": true}}, "queries": page,
		"records_total": len(rows), "records_filtered": len(filtered)}
	if page == nil {
		resp["queries"] = []any{}
	}
	if len(page) == limit {
		if last := page[len(page)-1]["id"].(int); last > 1 {
			resp["cursor_next"] = strconv.Itoa(last)
		}
	}
	f.writeJSON(w, 200, resp)
}

func (f *fakeAPI) queriesStream(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	n := f.streamConn
	f.streamConn++
	f.lastIDs = append(f.lastIDs, r.Header.Get("Last-Event-ID"))
	var batch []string
	if n < len(f.stream) {
		batch = f.stream[n]
	}
	last := n >= len(f.stream)-1
	f.mu.Unlock()
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(200)
	fl := w.(http.Flusher)
	_, _ = fmt.Fprint(w, ": hello\nretry: 10\n\n")
	for _, ev := range batch {
		_, _ = fmt.Fprint(w, ev)
		fl.Flush()
	}
	if last {
		<-r.Context().Done()
	}
}
