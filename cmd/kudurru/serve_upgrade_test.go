package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// releasedV01State puts a golden state.db of the 0.1.0 release
// (internal/store/sqlite/testdata/v0.1.0) into the data directory of cfg and
// returns its path and checksum.
func releasedV01State(t *testing.T, cfg *config.Config, name string) (string, [32]byte) {
	t.Helper()
	f, err := os.Open(filepath.Join("..", "..", "internal", "store", "sqlite", "testdata", "v0.1.0", name)) //nolint:gosec // G304: a committed test fixture
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(io.LimitReader(zr, 64<<20))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cfg.Node.DataDir, sqlite.FileName)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path, sha256.Sum256(raw)
}

// upgradeMountedRequest is controlMountedRequest for a server that is still
// publishing what the upgrade and the controller write: an attempt that
// arrives between two generations is answered 503 before any handler runs,
// and is made again. The requests of this test are a login and writes that
// must be refused, so a new attempt can never store anything twice.
func upgradeMountedRequest(t *testing.T, client *http.Client, base, method, path, csrf string, body any, want int, out any) {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		if encoded, err = json.Marshal(body); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), method, base+path, bytes.NewReader(encoded))
		if err != nil {
			t.Fatal(err)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if csrf != "" {
			req.Header.Set("X-Kudurru-CSRF", csrf)
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", method, path, err)
		}
		data, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
		_ = response.Body.Close()
		if response.StatusCode == http.StatusServiceUnavailable && want != http.StatusServiceUnavailable && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil || response.StatusCode != want {
			t.Fatalf("%s %s: status %d, want %d: %s (%v)", method, path, response.StatusCode, want, data, err)
		}
		if out != nil {
			if err := json.Unmarshal(data, out); err != nil {
				t.Fatalf("%s %s: %v", method, path, err)
			}
		}
		return
	}
}

// upgradeMountedConfig is the mounted configuration for a data directory of
// the 0.1.0 release: no static provider or TSIG file of the fixture's own.
func upgradeMountedConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	cfg.TSIGKeys = nil
	return cfg
}

// TestRunServeSQLiteUpgradeWithLegacyPlaceholders mounts the complete server
// on state.db files written by the released 0.1.0 binary.
//
// The file with the objects 0.1.0 accepted and this release cannot serve
// (zones of the types it reserved, a second DNSZone for a zone name, a
// free-form policy rewrite) is not upgraded: runServe returns the refusal
// that names them and tells the operator what to do, before any listener
// opens, and the file is byte for byte what 0.1.0 wrote. Before the refusal
// existed the same start upgraded the schema and then served nothing: no
// DNS answer, readiness 503 and every API route 503, with no way back.
//
// The ordinary file is upgraded and served: DNS answers what 0.1.0 stored,
// readiness is 200, the admin of 0.1.0 logs in with the password it had, and
// a write that would store such an object again is refused as invalid while
// the node keeps serving.
func TestRunServeSQLiteUpgradeWithLegacyPlaceholders(t *testing.T) {
	t.Run("legacy objects refuse the upgrade", func(t *testing.T) {
		cfg := upgradeMountedConfig(t)
		path, sum := releasedV01State(t, cfg, "state-legacy.db.gz")
		done := make(chan error, 1)
		go func() { done <- runServe(t.Context(), cfg, "") }()
		var err error
		select {
		case err = <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("runServe kept running on a data directory it cannot upgrade")
		}
		var upgrade *sqlite.UpgradeError
		if !errors.Is(err, sqlite.ErrUpgradeRefused) || !errors.As(err, &upgrade) {
			t.Fatalf("runServe = %v, want the upgrade refusal", err)
		}
		if upgrade.From != 1 || upgrade.To != store.SchemaVersion || len(upgrade.Issues) != 8 {
			t.Fatalf("refusal of an upgrade from %d to %d with %d objects", upgrade.From, upgrade.To, len(upgrade.Issues))
		}
		for _, want := range []string{
			"state.db was written by an earlier kudurru",
			"DNSZone legacy-secondary: spec: Invalid value: secondary zones require secondary configuration",
			"DNSZone legacy-stub: spec: Invalid value: stub zones require nameserver configuration",
			`DNSZone legacy-external: spec.type: Unsupported value: "external"`,
			`DNSZone legacy-duplicate: spec.zoneName: Duplicate value: "API.test.": the zone is also configured by DNSZone "api-test"`,
			"Policy default/default: spec.rewrites[0]:",
			`DNSZone legacy-grant-outside: spec.policy.updatePolicies[0].name: Invalid value: "host.elsewhere.test."`,
			`DNSZone legacy-grant-type: spec.policy.updatePolicies[0].types: Invalid value: "BOGUS"`,
			"state.db was not changed",
			"Start that earlier release on this data directory, correct or delete the listed objects",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal lacks %q:\n%v", want, err)
			}
		}
		after, readErr := os.ReadFile(path) //nolint:gosec // the test's data directory
		if got := sha256.Sum256(after); readErr != nil || !bytes.Equal(got[:], sum[:]) {
			t.Fatalf("the refused upgrade changed state.db (%v)", readErr)
		}
		// Nothing listens: the refusal comes before the first socket.
		if response, err := (&http.Client{Timeout: time.Second}).Get("http://" + cfg.Listeners.Ops.Address + "/healthz/ready"); err == nil {
			_ = response.Body.Close()
			t.Fatal("the ops listener answers after a refused upgrade")
		}
	})

	t.Run("an ordinary directory is upgraded and served", func(t *testing.T) {
		cfg := upgradeMountedConfig(t)
		// The block list below whose URL never answers is on loopback.
		cfg.Blocking.AllowLocalListURLs = true
		releasedV01State(t, cfg, "state.db.gz")
		// A list URL that accepts the connection and never answers, until
		// the test ends.
		var asked atomic.Int32
		silent := make(chan struct{})
		hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			asked.Add(1)
			select {
			case <-silent:
			case <-r.Context().Done():
			}
		}))
		t.Cleanup(hanging.Close)
		t.Cleanup(func() { close(silent) })
		transport := &http.Transport{}
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		client := &http.Client{Jar: jar, Transport: transport, Timeout: 10 * time.Second}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		t.Cleanup(func() {
			transport.CloseIdleConnections()
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("shutdown after the upgrade: %v", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("shutdown after the upgrade did not join")
			}
		})
		ready := func() int {
			response, err := client.Get("http://" + cfg.Listeners.Ops.Address + "/healthz/ready")
			if err != nil {
				return 0
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			return response.StatusCode
		}
		deadline := time.Now().Add(30 * time.Second)
		for ready() != http.StatusOK {
			select {
			case err := <-done:
				done <- err
				t.Fatalf("runServe ended on the 0.1.0 data directory: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("the upgraded server is not ready")
			}
		}
		// A query in flight when a generation is published is dropped
		// (docs/architecture/query-path.md); a resolver asks again.
		answer := func(name string) string {
			t.Helper()
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			var last string
			for range 150 {
				r, _, err := (&dns.Client{Timeout: time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
				if err == nil && r.Rcode == dns.RcodeSuccess && len(r.Answer) == 1 {
					return r.Answer[0].(*dns.A).A.String()
				}
				if err != nil {
					last = err.Error()
				} else {
					last = dns.RcodeToString[r.Rcode]
				}
				time.Sleep(100 * time.Millisecond)
			}
			t.Fatalf("%s: %s", name, last)
			return ""
		}
		// The zone created through the API of 0.1.0 and its records.
		if got := answer("www.api.test."); got != "192.0.2.10" {
			t.Fatalf("www.api.test. = %s, 0.1.0 stored 192.0.2.10", got)
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d after the first answers", got)
		}
		// The admin of 0.1.0, with the password of the fixture
		// (internal/store/sqlite/testdata/v0.1.0/README.md).
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		var session api.SessionInfo
		upgradeMountedRequest(t, client, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "upgrade-e2e-password-1"}, http.StatusOK, &session) // #nosec G101 -- the documented password of a committed test fixture
		zone := func(name, zoneName, typ string) map[string]any {
			return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": name}, "spec": map[string]any{"zoneName": zoneName, "type": typ}}
		}
		for name, body := range map[string]map[string]any{
			"a zone type that is not served":       zone("ext", "ext.test.", "external"),
			"a second DNSZone for one zone":        zone("again", "API.test.", "primary"),
			"an update mode that does not compile": {"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "upd"}, "spec": map[string]any{"zoneName": "upd.test.", "type": "primary", "nameservers": []string{"ns1.upd.test."}, "policy": map[string]any{"update": "useNetworkACL"}}},
			"a zone policy that cannot compile":    {"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "acl"}, "spec": map[string]any{"zoneName": "acl.test.", "type": "primary", "policy": map[string]any{"queryAccess": "useNetworkACL", "queryACL": []string{"10.0.0.0/33"}}}},
		} {
			var problem struct {
				Error api.ErrorBody `json:"error"`
			}
			upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", session.CSRF, body, http.StatusBadRequest, &problem)
			if problem.Error.Key != api.KeyInvalid || len(problem.Error.Fields) == 0 {
				t.Fatalf("%s: refused as %q naming %v", name, problem.Error.Key, problem.Error.Fields)
			}
		}
		// The refusals stored nothing, so the server still has a generation.
		if got := answer("www.api.test."); got != "192.0.2.10" {
			t.Fatalf("www.api.test. = %s after the refused writes", got)
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d after the refused writes", got)
		}
		upgradeMountedRequest(t, client, base, http.MethodGet, "/objects/dnszones/api-test", "", nil, http.StatusOK, nil)

		// What 0.1.0 stored and served around is stored again on the
		// upgraded node, and every later write is still served (D373): a
		// block list whose URL never downloads is served empty, and the
		// endpoints the zone builder refuses are left out of their zone.
		object := func(kind, name, namespace string, spec map[string]any) map[string]any {
			meta := map[string]any{"name": name}
			if namespace != "" {
				meta["namespace"] = namespace
			}
			return map[string]any{"apiVersion": "kudurru.io/v1alpha1", "kind": kind, "metadata": meta, "spec": spec}
		}
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/blocklists", session.CSRF, object("BlockList", "dead-after-upgrade", "default", map[string]any{
			"sources": []any{map[string]any{"name": "remote", "format": "domains", "url": "http://127.0.0.1:9/never-after.txt"}},
		}), http.StatusCreated, nil)
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnsrecordsets", session.CSRF, object("DNSRecordSet", "refused-after-upgrade", "default", map[string]any{
			"zoneRef": map[string]any{"name": "api-test"}, "endpoints": []any{
				map[string]any{"dnsName": "api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}},
				map[string]any{"dnsName": "api.test", "recordType": "NS", "targets": []any{"ns2.elsewhere.test."}},
				map[string]any{"dnsName": "www.api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}},
				map[string]any{"dnsName": "kept-after.api.test", "recordType": "A", "targets": []any{"192.0.2.78"}},
			},
		}), http.StatusCreated, nil)
		if got := answer("kept-after.api.test."); got != "192.0.2.78" {
			t.Fatalf("kept-after.api.test. = %s", got)
		}
		if got := answer("www.api.test."); got != "192.0.2.10" {
			t.Fatalf("www.api.test. = %s next to a refused CNAME", got)
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d with a block list that never downloaded", got)
		}

		// A record set that is invalid as a whole, of every kind 0.1.0
		// stored and reported Invalid, and a CNAME and an A record for one
		// name in one record set: the zone keeps being served and a later
		// record is served next to them (D373).
		for name, endpoints := range map[string][]any{
			"not-an-address": {map[string]any{"dnsName": "bad-a.api.test", "recordType": "A", "targets": []any{"not-an-ip"}}},
			"two-for-a-name": {map[string]any{"dnsName": "twice.api.test", "recordType": "A", "targets": []any{"192.0.2.81"}}, map[string]any{"dnsName": "twice.api.test", "recordType": "A", "targets": []any{"192.0.2.82"}}},
			"unknown-type":   {map[string]any{"dnsName": "bogus.api.test", "recordType": "BOGUS", "targets": []any{"x"}}},
			"mx-no-pref":     {map[string]any{"dnsName": "mail.api.test", "recordType": "MX", "targets": []any{"mx.api.test."}}},
			"cname-and-a":    {map[string]any{"dnsName": "pair.api.test", "recordType": "CNAME", "targets": []any{"elsewhere.test."}}, map[string]any{"dnsName": "pair.api.test", "recordType": "A", "targets": []any{"192.0.2.83"}}},
		} {
			upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnsrecordsets", session.CSRF, object("DNSRecordSet", "invalid-"+name, "default", map[string]any{
				"zoneRef": map[string]any{"name": "api-test"}, "endpoints": endpoints,
			}), http.StatusCreated, nil)
		}
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnsrecordsets", session.CSRF, object("DNSRecordSet", "after-invalid", "default", map[string]any{
			"zoneRef": map[string]any{"name": "api-test"}, "endpoints": []any{map[string]any{"dnsName": "after-invalid.api.test", "recordType": "A", "targets": []any{"192.0.2.84"}}},
		}), http.StatusCreated, nil)
		if got := answer("after-invalid.api.test."); got != "192.0.2.84" {
			t.Fatalf("after-invalid.api.test. = %s", got)
		}
		if got := answer("www.api.test."); got != "192.0.2.10" {
			t.Fatalf("www.api.test. = %s next to the invalid record sets", got)
		}
		for _, name := range []string{"twice.api.test.", "pair.api.test."} {
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeA)
			if r, _, err := (&dns.Client{Timeout: time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address); err != nil || len(r.Answer) != 0 && r.Answer[0].Header().Rrtype == dns.TypeA {
				t.Fatalf("%s A of a refused endpoint: %v %v", name, r, err)
			}
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d with invalid record sets", got)
		}
		// Each is reported on its record set.
		for _, name := range []string{"not-an-address", "two-for-a-name", "unknown-type", "mx-no-pref"} {
			var last string
			deadline := time.Now().Add(30 * time.Second)
			for {
				var rs struct {
					Status struct {
						Conditions []struct {
							Type, Status, Reason string
						} `json:"conditions"`
					} `json:"status"`
				}
				upgradeMountedRequest(t, client, base, http.MethodGet, "/objects/dnsrecordsets/default/invalid-"+name, "", nil, http.StatusOK, &rs)
				reported := false
				for _, c := range rs.Status.Conditions {
					reported = reported || c.Type == "Ready" && c.Status == "False" && c.Reason == "Invalid"
				}
				if reported {
					break
				}
				last = fmt.Sprintf("%+v", rs.Status.Conditions)
				if time.Now().After(deadline) {
					t.Fatalf("record set invalid-%s is not reported Invalid: %s", name, last)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}

		// Update grants the primary runtime cannot compile are refused at
		// write, also for a zone that denies updates (D373).
		for name, policy := range map[string]map[string]any{
			"a grant outside the zone": {"update": "deny", "updatePolicies": []any{map[string]any{"tsigKeyRef": "ddns", "name": "host.elsewhere.test."}}},
			"a grant of no type":       {"update": "allow", "updatePolicies": []any{map[string]any{"tsigKeyRef": "ddns", "name": "grant.test.", "types": []any{"BOGUS"}}}},
		} {
			var problem struct {
				Error api.ErrorBody `json:"error"`
			}
			upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", session.CSRF, object("DNSZone", "grant", "", map[string]any{
				"zoneName": "grant.test.", "type": "primary", "nameservers": []any{"ns1.grant.test."}, "policy": policy,
			}), http.StatusBadRequest, &problem)
			if problem.Error.Key != api.KeyInvalid || !strings.Contains(strings.Join(problem.Error.Fields, " "), "spec.policy.updatePolicies[0]") {
				t.Fatalf("%s: refused as %q naming %v", name, problem.Error.Key, problem.Error.Fields)
			}
		}

		// A block list whose URL accepts the connection and never answers
		// does not hold back the serving generation while its first
		// download runs: DNS, readiness and later writes go on (D373).
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/blocklists", session.CSRF, object("BlockList", "silent-url", "default", map[string]any{
			"sources": []any{map[string]any{"name": "remote", "format": "domains", "url": hanging.URL + "/silent.txt"}},
		}), http.StatusCreated, nil)
		for deadline := time.Now().Add(30 * time.Second); asked.Load() == 0; time.Sleep(50 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("the fetcher never asked the silent URL")
			}
		}
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnsrecordsets", session.CSRF, object("DNSRecordSet", "while-silent", "default", map[string]any{
			"zoneRef": map[string]any{"name": "api-test"}, "endpoints": []any{map[string]any{"dnsName": "while-silent.api.test", "recordType": "A", "targets": []any{"192.0.2.85"}}},
		}), http.StatusCreated, nil)
		if got := answer("while-silent.api.test."); got != "192.0.2.85" {
			t.Fatalf("while-silent.api.test. = %s", got)
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d while a list URL does not answer", got)
		}
		upgradeMountedRequest(t, client, base, http.MethodGet, "/zones", "", nil, http.StatusOK, nil)
		var silentList struct {
			Status struct {
				Sources []struct {
					Digest string `json:"digest"`
				} `json:"sources"`
			} `json:"status"`
		}
		upgradeMountedRequest(t, client, base, http.MethodGet, "/objects/blocklists/default/silent-url", "", nil, http.StatusOK, &silentList)
		if len(silentList.Status.Sources) > 0 && silentList.Status.Sources[0].Digest != "" {
			t.Fatalf("the silent URL has a digest: %+v", silentList.Status)
		}

		// The lifecycle of a deleted zone keeps its name: a zone for it
		// under another object name is refused, naming the object to use,
		// and the node keeps serving. The same object name is accepted.
		soa := func(name string) bool {
			q := new(dns.Msg)
			q.SetQuestion(name, dns.TypeSOA)
			r, _, err := (&dns.Client{Timeout: time.Second}).ExchangeContext(t.Context(), q, cfg.Listeners.DNS[0].Address)
			return err == nil && r.Rcode == dns.RcodeSuccess && r.Authoritative && len(r.Answer) == 1
		}
		temp := object("DNSZone", "temp", "", map[string]any{"zoneName": "temp.test.", "type": "primary", "nameservers": []any{"ns1.temp.test."}})
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", session.CSRF, temp, http.StatusCreated, nil)
		served := func(want bool) {
			t.Helper()
			deadline := time.Now().Add(30 * time.Second)
			for soa("temp.test.") != want {
				if time.Now().After(deadline) {
					t.Fatalf("temp.test. served = %v after 30s", !want)
				}
				time.Sleep(100 * time.Millisecond)
			}
		}
		served(true)
		upgradeMountedRequest(t, client, base, http.MethodDelete, "/objects/dnszones/temp", session.CSRF, nil, http.StatusNoContent, nil)
		served(false)
		var problem struct {
			Error api.ErrorBody `json:"error"`
		}
		renamed := object("DNSZone", "temp-renamed", "", map[string]any{"zoneName": "temp.test.", "type": "primary", "nameservers": []any{"ns1.temp.test."}})
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", session.CSRF, renamed, http.StatusBadRequest, &problem)
		if problem.Error.Key != api.KeyInvalid || !strings.Contains(strings.Join(problem.Error.Fields, " "), "spec.zoneName") || !strings.Contains(problem.Error.Message, `DNSZone "temp"`) {
			t.Fatalf("a zone under another name for a deleted zone: %q %v %s", problem.Error.Key, problem.Error.Fields, problem.Error.Message)
		}
		if got := answer("www.api.test."); got != "192.0.2.10" {
			t.Fatalf("www.api.test. = %s after the refused zone", got)
		}
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d after the refused zone", got)
		}
		upgradeMountedRequest(t, client, base, http.MethodPost, "/objects/dnszones", session.CSRF, temp, http.StatusCreated, nil)
		served(true)
		if got := ready(); got != http.StatusOK {
			t.Fatalf("readiness %d after the zone was created again", got)
		}
	})
}
