package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
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
	f, err := os.Open(filepath.Join("..", "..", "internal", "store", "sqlite", "testdata", "v0.1.0", name))
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
		if upgrade.From != 1 || upgrade.To != store.SchemaVersion || len(upgrade.Issues) != 6 {
			t.Fatalf("refusal of an upgrade from %d to %d with %d objects", upgrade.From, upgrade.To, len(upgrade.Issues))
		}
		for _, want := range []string{
			"state.db was written by an earlier kudurru",
			"DNSZone legacy-secondary: spec: Invalid value: secondary zones require secondary configuration",
			"DNSZone legacy-stub: spec: Invalid value: stub zones require nameserver configuration",
			`DNSZone legacy-external: spec.type: Unsupported value: "external"`,
			`DNSZone legacy-duplicate: spec.zoneName: Duplicate value: "API.test.": the zone is also configured by DNSZone "api-test"`,
			"Policy default/default: spec.rewrites[0]:",
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
		releasedV01State(t, cfg, "state.db.gz")
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
			"a zone type that is not served":    zone("ext", "ext.test.", "external"),
			"a second DNSZone for one zone":     zone("again", "API.test.", "primary"),
			"a zone policy that cannot compile": {"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone", "metadata": map[string]any{"name": "acl"}, "spec": map[string]any{"zoneName": "acl.test.", "type": "primary", "policy": map[string]any{"queryAccess": "useNetworkACL", "queryACL": []string{"10.0.0.0/33"}}}},
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
	})
}
