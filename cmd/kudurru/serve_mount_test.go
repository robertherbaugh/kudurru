package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/prometheus/client_golang/prometheus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/keys"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Exercise the actual serve command rather than rebuilding its assembly in a
// fixture. Dynamic records must be published by the complete runtime and survive
// restart without the legacy desired-record compiler or notifier overwriting it.
func TestRunServeSQLiteCompletePrimaryUpdateRestart(t *testing.T) {
	cfg, secret := mountedTestConfig(t)
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "default"}},
		&public.Policy{ObjectMeta: metav1.ObjectMeta{Name: "mounted-diagnosis", Namespace: "default"}, Spec: public.PolicySpec{ClientGroupRefs: []string{"absent-mounted-group"}}},
		&public.BlockList{ObjectMeta: metav1.ObjectMeta{Name: "mounted-source", Namespace: "default"}, Spec: public.BlockListSpec{Sources: []public.ListSource{{Name: "entries", Format: "domains", Inline: []string{"blocked.example", "excluded.example"}, Exclude: []string{"excluded.example"}}}}},
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{
			ZoneName: "example.", Type: public.ZoneTypePrimary, Nameservers: []string{"ns.example."},
			Policy: public.ZonePolicy{Notify: public.NotifyNone, Update: public.AccessAllow, UpdatePolicies: []public.UpdatePolicy{{TSIGKeyRef: "updates", Name: "*.example.", Types: []string{"A"}}}},
		}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "seed", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "seed.example.", RecordType: "A", Targets: []string{"192.0.2.42"}}}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var apiToken string
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, "") }()
		stopped := false
		stop := func() {
			if stopped {
				return
			}
			stopped = true
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Error("serve ended", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("serve failed to join shutdown")
			}
		}
		t.Cleanup(stop)
		awaitMountedDNS(t, cfg, "seed.example.", "192.0.2.42")
		if restart == 0 {
			for _, network := range []string{"udp", "tcp"} {
				client := &dns.Client{Net: network, Timeout: 5 * time.Second, TsigSecret: map[string]string{"updates.": secret}}
				deadline := time.Now().Add(10 * time.Second)
				for {
					// This is an idempotent RR addition in a NEW native request,
					// not replay/dedup of a retained verification capability.
					msg := primaryUpdateMessage("example.", "dynamic.example.", true)
					answer, _, err := client.Exchange(msg, cfg.Listeners.DNS[0].Address)
					if err != nil || answer == nil || answer.IsTsig() == nil {
						t.Fatalf("mounted %s UPDATE failed verification: %v", network, err)
					}
					if answer.Rcode == dns.RcodeSuccess {
						break
					}
					if answer.Rcode != dns.RcodeServerFailure || !time.Now().Before(deadline) {
						t.Fatalf("mounted %s UPDATE failed: rcode %d", network, answer.Rcode)
					}
					time.Sleep(25 * time.Millisecond)
				}
			}
		}
		awaitMountedDNS(t, cfg, "dynamic.example.", "192.0.2.33")
		apiToken = verifyMountedHTTP(t, cfg, restart, apiToken)
		stop()
	}
}

func mountedTestConfig(t *testing.T) (*config.Config, string) {
	t.Helper()
	cfg, err := config.Load("", nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store.Driver = "sqlite"
	cfg.Node.DataDir = t.TempDir()
	cfg.Node.Name = "mount-test"
	cfg.Node.Lameduck = 0
	cfg.Listeners.DNS = []config.DNSListener{{Address: offlineFreeAddress(t, "tcp"), Protocols: []string{"udp", "tcp"}}}
	cfg.Listeners.Sockets = 1
	cfg.Listeners.API.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.API.TLS.Auto = config.APITLSOff
	cfg.Listeners.Ops.Address = offlineFreeAddress(t, "tcp")
	cfg.Listeners.Metrics.Address = "127.0.0.1:0"
	cfg.Listeners.Internal.Address = "127.0.0.1:0"
	cfg.Auth.AdminSocket.Path = ""
	cfg.Auth.Session.Secure = "never"
	// Provider acquisition belongs to the complete background owner. Legacy
	// bootstrap/admin construction must not read this absent credential file.
	cfg.Auth.IdentityProviders = []map[string]any{{"id": "corp", "issuer": "https://issuer.example", "clientID": "browser", "clientSecretFile": filepath.Join(t.TempDir(), "absent-secret"), "redirectURL": "https://dns.example/api/v1/auth/oidc/corp/callback"}}
	cfg.ObjectsDir = ""
	cfg.Querylog.Enabled = false
	cfg.Log.Level = "error"
	t.Setenv(keys.EnvKey, base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32)))
	t.Setenv(keys.EnvKey+"_FILE", "")
	t.Setenv(keys.EnvPreviousKey, "")
	t.Setenv(keys.EnvPreviousKey+"_FILE", "")
	t.Setenv(users.EnvAdminPassword, "mounted-test-password-1")
	secret := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	keyFile := filepath.Join(t.TempDir(), "update-key")
	if err := os.WriteFile(keyFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg.TSIGKeys = []config.TSIGKey{{Name: "updates.", Algorithm: dns.HmacSHA256, SecretFile: keyFile}}
	return cfg, secret
}

// Real controller status publications may supersede a request between capture
// and current command entry. A new HTTP attempt must capture again; production
// retains the original proof and correctly refuses the stale attempt. These
// fixture requests tolerate duplicate enrollment after an ambiguous 503 and
// claim no cross-request deduplication.
type mountedRetryTransport struct{}

func (mountedRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	deadline := time.Now().Add(5 * time.Second)
	for {
		response, err := http.DefaultTransport.RoundTrip(req)
		if err != nil || response.StatusCode != http.StatusServiceUnavailable || !time.Now().Before(deadline) {
			return response, err
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-time.After(25 * time.Millisecond):
		}
		next := req.Clone(req.Context())
		if req.GetBody != nil {
			next.Body, err = req.GetBody()
			if err != nil {
				return nil, err
			}
		}
		req = next
	}
}

func TestRunServeSQLiteFollowerWaitsWithoutBootstrap(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	opts := sqlite.Options{Scheme: scheme, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: cfg.Node.Name}
	path := filepath.Join(cfg.Node.DataDir, sqlite.FileName)
	peer, err := sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	term := replication.Term{ClusterID: "mount-cluster", Epoch: 1, PrimaryID: "primary", PromotionID: "initial"}
	if err := peer.Enroll(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	before := peer.Authority()
	if err := peer.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, "") }()
	t.Cleanup(cancel)
	deadline := time.Now().Add(10 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.API.Address, "/api/v1/auth/status") != http.StatusServiceUnavailable {
		select {
		case err := <-done:
			t.Fatal("follower startup ended before initial image", err)
		case <-time.After(20 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("follower never exposed denied pending HTTP")
		}
	}
	// Cross the host's polling interval while the follower has no canonical
	// image. Authentication and readiness remain denied without ending Run.
	select {
	case err := <-done:
		t.Fatal("pending follower terminated", err)
	case <-time.After(1200 * time.Millisecond):
	}
	if code := offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready"); code != http.StatusServiceUnavailable {
		t.Fatal("follower ready without first image", code)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("follower failed to join shutdown")
	}
	peer, err = sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = peer.Close() }()
	if !reflect.DeepEqual(before, peer.Authority()) {
		t.Fatal("follower startup changed durable authority")
	}
	if _, err := peer.ReadGeneration(t.Context()); !errors.Is(err, replication.ErrReadOnly) {
		t.Fatal("follower bootstrap invented an initial image", err)
	}
	for _, kind := range []store.Kind{"Kudurru", "DNSZone", "LocalUser", "ServiceAccount"} {
		list, err := peer.List(t.Context(), kind, store.ListOptions{})
		if err != nil || len(list.Items) != 0 {
			t.Fatal("follower bootstrap wrote objects", kind, err)
		}
	}
}

func verifyMountedHTTP(t *testing.T, cfg *config.Config, restart int, token string) string {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &contractClient{t: t, base: "http://" + cfg.Listeners.API.Address + "/api/v1", http: &http.Client{Jar: jar, Timeout: 10 * time.Second, Transport: mountedRetryTransport{}}}
	var login api.SessionInfo
	client.call(http.MethodPost, "/auth/login", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, 200, &login)
	if login.CSRF == "" || login.Principal.Name != "admin" || login.MustChangePassword {
		t.Fatal("mounted login did not issue the configured admin session")
	}
	client.csrf = login.CSRF
	var me api.Me
	client.call(http.MethodGet, "/auth/me", nil, 200, &me)
	if me.Principal.Name != "admin" || len(me.Roles) == 0 {
		t.Fatal("mounted /me lost captured grants")
	}
	var catalog api.CatalogueInfo
	client.call(http.MethodGet, "/catalogue", nil, http.StatusOK, &catalog)
	if catalog.Origin == "" || catalog.Digest == "" || catalog.Services == 0 || len(catalog.Sources) == 0 || catalog.Refresh != nil {
		t.Fatal("mounted catalog lost captured contents/provenance or sampled live refresh status")
	}
	var sourceEntries api.SourceEntries
	client.call(http.MethodGet, "/blocklists/default/mounted-source/sources/entries/entries", nil, http.StatusOK, &sourceEntries)
	if sourceEntries.Kind != "inline" || sourceEntries.Total != 2 || sourceEntries.Excluded != 1 || len(sourceEntries.Items) != 2 || sourceEntries.Items[0].Value != "blocked.example" || !sourceEntries.Items[0].Enabled || sourceEntries.Items[1].Enabled || sourceEntries.Fetched != nil || sourceEntries.Stale {
		t.Fatal("mounted source GET lost captured values/exclusions or invented fetch provenance")
	}
	var policyState api.PolicyState
	client.call(http.MethodGet, "/policies/default/default/state", nil, http.StatusOK, &policyState)
	if policyState.Policy != "default/default" || !policyState.Enabled || !policyState.Active || policyState.Lists == nil || policyState.Services == nil {
		t.Fatal("mounted policy state lost captured default policy or complete diagnostics")
	}
	var blocking api.BlockingState
	client.call(http.MethodGet, "/dns/blocking", nil, http.StatusOK, &blocking)
	if blocking.Scope != "cluster" || blocking.Configured != cfg.Blocking.Enabled || blocking.Blocking != blocking.Configured || blocking.Until != nil || blocking.Timer != nil {
		t.Fatal("mounted blocking state did not come from the canonical publication")
	}
	var upstreams api.UpstreamsBody
	client.call(http.MethodGet, "/stats/upstreams", nil, http.StatusOK, &upstreams)
	if len(upstreams.Servers) == 0 {
		t.Fatal("mounted upstream diagnostics omitted the captured resolver set")
	}
	var sessions api.AuthList[api.SessionRecordInfo]
	client.call(http.MethodGet, "/auth/sessions", nil, 200, &sessions)
	current := ""
	for _, session := range sessions.Items {
		if session.Current && !session.Revoked && session.ExpiresAt.After(session.IssuedAt) {
			current = session.ID
		}
	}
	if current == "" {
		t.Fatal("mounted session inventory lacks the actual enrollment")
	}
	if restart == 0 {
		var notify api.NotifyResult
		client.call(http.MethodPost, "/zones/home/notify", nil, http.StatusAccepted, &notify)
		if notify.Zone != "home" || len(notify.Targets) != 0 {
			t.Fatal("policy-none NOTIFY lost its current no-op result")
		}
		// Convenience routes without current admission must never reach their
		// legacy live-Store writers because other commands are mounted.
		blocked := *client
		blocked.http = &http.Client{Jar: jar, Timeout: 5 * time.Second}
		// These legacy aliases can write several lists/policies or return an
		// unchecked no-op. They need their own admitted command adapters.
		blocked.call(http.MethodPost, "/allowlist", map[string]any{"values": []string{"escape.example"}}, 503, nil)
		blocked.call(http.MethodPost, "/denylist:batch", nil, 503, nil)
		blocked.call(http.MethodPost, "/domains/allow/exact", nil, 503, nil)
		blocked.call(http.MethodPatch, "/blocklists/default/starter/sources/0/entries", map[string]any{}, 503, nil)
		blocked.call(http.MethodPost, "/unblockrequests/default/request/approve", nil, 503, nil)
		var created api.CreatedToken
		client.call(http.MethodPost, "/auth/tokens", api.CreateTokenRequest{Description: "mounted", ExpiresIn: "1h", Scopes: []string{"viewer"}}, 201, &created)
		token = created.Token
		if !strings.HasPrefix(token, "kdr_") || created.Owner.UID == "" {
			t.Fatal("mounted token did not bind actual account identity")
		}
		awaitMountedDNS(t, cfg, "dynamic.example.", "192.0.2.33")
		body := map[string]any{"apiVersion": public.GroupVersion.String(), "kind": "DNSRecordSet", "metadata": map[string]any{"name": "http", "namespace": "default"}, "spec": map[string]any{"zoneRef": map[string]any{"name": "home"}, "endpoints": []any{map[string]any{"dnsName": "http.example.", "recordType": "A", "targets": []string{"192.0.2.44"}}}}}
		client.call(http.MethodPut, "/objects/dnsrecordsets/default/http", body, 201, nil)
		awaitMountedDNS(t, cfg, "http.example.", "192.0.2.44")
		var settings api.SettingsBody
		client.call(http.MethodPatch, "/settings", map[string]any{"cache": map[string]any{"maxEntries": 42}}, 200, &settings)
		if settings.Persisted == nil || !*settings.Persisted {
			t.Fatal("mounted settings were not acknowledged durable")
		}
	}
	awaitMountedDNS(t, cfg, "http.example.", "192.0.2.44")
	var desiredRecords api.ObjectList
	client.call(http.MethodGet, "/objects/dnsrecordsets?namespace=default", nil, 200, &desiredRecords)
	foundDesired := false
	for _, record := range desiredRecords.Items {
		if metadata, ok := record["metadata"].(map[string]any); ok && metadata["name"] == "http" && metadata["uid"] != "" {
			foundDesired = true
		}
	}
	if !foundDesired {
		t.Fatal("mounted captured object list lost the committed record")
	}
	var record map[string]any
	client.call(http.MethodGet, "/objects/dnsrecordsets/default/http", nil, 200, &record)
	if record["metadata"].(map[string]any)["name"] != "http" {
		t.Fatal("mounted captured object detail changed identity")
	}
	var settings api.SettingsBody
	client.call(http.MethodGet, "/settings", nil, 200, &settings)
	if settings.Settings["cache"].(map[string]any)["maxEntries"] != float64(42) {
		t.Fatal("mounted canonical settings did not survive publication/restart")
	}
	// A token issued before restart must read the committed dynamic records,
	// while its viewer scope cannot enter an actual current settings command.
	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequestWithContext(t.Context(), method, client.base+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		response, err := client.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = response.Body.Close() }()
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response.StatusCode, raw
	}
	code, exported := request(http.MethodGet, "/zones/home/export", "")
	if code != 200 || !bytes.Contains(exported, []byte("dynamic.example.")) || !bytes.Contains(exported, []byte("192.0.2.33")) || !bytes.Contains(exported, []byte("192.0.2.44")) {
		t.Fatalf("mounted committed export missing actual records (status %d)", code)
	}
	code, raw := request(http.MethodGet, "/zones/home/records", "")
	var records api.List[api.ZoneRecord]
	if code != 200 || json.Unmarshal(raw, &records) != nil {
		t.Fatal("mounted committed record listing failed", code)
	}
	found := false
	for _, record := range records.Items {
		if dns.Fqdn(record.DNSName) == "dynamic.example." && record.RecordType == "A" {
			found = true
			if record.RecordSet != "" || record.ManagedBy != "" {
				t.Fatal("dynamic UPDATE record was assigned invented desired ownership")
			}
		}
	}
	if !found {
		t.Fatalf("record listing omitted the dynamic UPDATE: %s", raw)
	}
	code, raw = request(http.MethodGet, "/info/messages", "")
	var messages api.List[api.DiagnosisMessage]
	if code != http.StatusOK || json.Unmarshal(raw, &messages) != nil {
		t.Fatalf("mounted captured diagnoses failed (status %d)", code)
	}
	found = false
	for _, message := range messages.Items {
		if message.Source == "policy" && message.Reason == "MissingReference" && message.Object == "Policy/default/mounted-diagnosis" {
			found = message.Severity == "warning" && message.Node == cfg.Node.Name
		}
	}
	if !found {
		t.Fatal("mounted captured diagnoses omitted the original policy's missing reference")
	}
	// The built-in query must receive the original HTTP serving lease. In
	// complete mode a missing bridge fails closed, even with a usable Chain.
	for _, query := range []struct{ name, address string }{
		{"dynamic.example.", "192.0.2.33"},
		{"http.example.", "192.0.2.44"},
	} {
		body, err := json.Marshal(api.DNSQueryRequest{Name: query.name, Type: "A", Client: "127.0.0.1", Proto: "udp"})
		if err != nil {
			t.Fatal(err)
		}
		code, raw := request(http.MethodPost, "/dns/query", string(body))
		var answer api.DNSQueryResult
		if code != http.StatusOK || json.Unmarshal(raw, &answer) != nil {
			t.Fatalf("mounted built-in query failed (status %d)", code)
		}
		if answer.Rcode != "NOERROR" || answer.Status != "authoritative" || answer.Error != "" || !answer.Flags.AA || len(answer.Answer) != 1 {
			t.Fatalf("mounted built-in query lost committed authority: %+v", answer)
		}
		rr, err := dns.NewRR(answer.Answer[0])
		if err != nil {
			t.Fatal(err)
		}
		a, ok := rr.(*dns.A)
		if !ok || a.Hdr.Name != query.name || a.A.String() != query.address || answer.Client != "127.0.0.1" || answer.Node != cfg.Node.Name {
			t.Fatalf("mounted built-in query changed committed record or caller metadata: %+v", answer)
		}
	}
	if code, _ := request(http.MethodPatch, "/settings", `{"cache":{"maxEntries":99}}`); code != 403 {
		t.Fatal("viewer token reached settings mutation", code)
	}
	client.call(http.MethodDelete, "/auth/sessions/"+current, nil, 204, nil)
	awaitMountedDNS(t, cfg, "http.example.", "192.0.2.44")
	client.call(http.MethodGet, "/auth/me", nil, 401, nil)
	return token
}

func awaitMountedDNS(t *testing.T, cfg *config.Config, name, address string) {
	t.Helper()
	awaitMountedDNSResult(t, cfg, name, address, func(answer *dns.Msg) bool {
		if answer.Rcode != dns.RcodeSuccess || len(answer.Answer) != 1 {
			return false
		}
		rr, ok := answer.Answer[0].(*dns.A)
		return ok && rr.A.String() == address
	})
}

func awaitMountedDNSAbsent(t *testing.T, cfg *config.Config, name string) {
	t.Helper()
	awaitMountedDNSResult(t, cfg, name, "NXDOMAIN", func(answer *dns.Msg) bool {
		return answer.Rcode == dns.RcodeNameError && len(answer.Answer) == 0
	})
}

func awaitMountedDNSResult(t *testing.T, cfg *config.Config, name, expected string, matches func(*dns.Msg) bool) {
	t.Helper()
	// Complete publication can require another bounded driver attempt after a
	// concurrent status update. Allow those attempts under race instrumentation;
	// the production operation deadlines and all serving assertions stay intact.
	started := time.Now()
	deadline := started.Add(time.Minute)
	client := &dns.Client{Timeout: 200 * time.Millisecond}
	for {
		query := new(dns.Msg)
		query.SetQuestion(name, dns.TypeA)
		answer, _, err := client.Exchange(query, cfg.Listeners.DNS[0].Address)
		ready := offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready")
		if err == nil && answer != nil && answer.Authoritative && matches(answer) && ready == http.StatusOK {
			if elapsed := time.Since(started); elapsed > 5*time.Second {
				t.Logf("mounted DNS published %s after %s", name, elapsed.Round(time.Millisecond))
			}
			return
		}
		if time.Now().After(deadline) {
			var health string
			response, healthErr := (&http.Client{Timeout: time.Second}).Get("http://" + cfg.Listeners.Ops.Address + "/healthz/ready")
			if healthErr == nil {
				body, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
				_ = response.Body.Close()
				health = string(body)
			}
			t.Fatalf("mounted DNS never reached %s=%s: answer=%v err=%v readiness=%d details=%q", name, expected, answer, err, ready, health)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
