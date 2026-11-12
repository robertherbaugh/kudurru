package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/apiclient"
	"github.com/robertherbaugh/kudurru/internal/cli"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// seedWebhookRecordsMount stores the zone and the record set the file
// Integration publishes, before the server starts.
func seedWebhookRecordsMount(t *testing.T, cfg *config.Config) {
	t.Helper()
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, obj := range []store.Object{
		&public.DNSZone{ObjectMeta: metav1.ObjectMeta{Name: "home"}, Spec: public.DNSZoneSpec{Type: public.ZoneTypePrimary, ZoneName: "example.", Nameservers: []string{"ns.example."}, Policy: public.ZonePolicy{Notify: public.NotifyNone}}},
		&public.DNSRecordSet{ObjectMeta: metav1.ObjectMeta{Name: "published", Namespace: "default"}, Spec: public.DNSRecordSetSpec{ZoneRef: public.ObjectRef{Name: "home"}, Endpoints: []public.Endpoint{{DNSName: "web.example.", RecordType: "A", RecordTTL: 300, Targets: []string{"192.0.2.80"}}}}},
	} {
		if _, err := st.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal("seed webhook records objects", err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}

// The real serve command with a file webhook Integration that names domains
// (F60c-Q1, D380): licensed startup, the file source, the elected host and
// the TLS records peer publish the record set with its registry row, and
// the CLI reads, checks and lists actions of the Integration through the
// real API listener. A Tier-B action the peer does not advertise is refused.
func TestRunServeWebhookRecordsFileIntegrationAndCLI(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	token := filepath.Join(t.TempDir(), "records-token")
	if err := os.WriteFile(token, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := newWebhookRecordsPeer(t, token)
	cfg.Credentials.AllowedFiles = []string{token, peer.ca}
	cfg.ObjectsDir = t.TempDir()
	definition := &public.Integration{TypeMeta: metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Integration"}, ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Resync: metav1.Duration{Duration: time.Hour},
		Credentials: map[string]public.CredentialReference{"bearerToken": {File: token}, "ca": {File: peer.ca}},
		Webhook:     &public.WebhookIntegration{Endpoint: peer.server.URL, Domains: []string{"example"}}}}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ObjectsDir, "integration.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	seedWebhookRecordsMount(t, cfg)
	sign := trustTestKey(t)
	key := sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureIntegrations}})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- runServe(ctx, cfg, key) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error("webhook records mount shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("webhook records mount shutdown did not join")
		}
	})
	until := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatal("webhook records mount ended before readiness", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("webhook records mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The elected host publishes the file Integration's record set.
	for deadline := time.Now().Add(60 * time.Second); !webhookRecordsPublished(t, peer); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the file webhook Integration did not publish", peer.rows(t))
		}
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	base := "http://" + cfg.Listeners.API.Address + api.BasePath
	var login api.SessionInfo
	mountedFactorRequest(t, c, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
	var created api.CreatedToken
	mountedFactorRequest(t, c, base, http.MethodPost, "/auth/tokens", login.CSRF, api.CreateTokenRequest{Description: "webhook-records"}, http.StatusCreated, &created)
	if got := routerContractRequest(t, &http.Client{Timeout: 10 * time.Second}, http.MethodGet, base+"/auth/me", "", created.Token, nil); got.status != http.StatusOK {
		t.Fatal("new token never became usable", got.status, got.key)
	}
	contexts := filepath.Join(t.TempDir(), "contexts.yaml")
	// run executes one CLI command against the listener. Only the transient
	// generation_pending answer, given before any command runs, is observed
	// again.
	run := func(args ...string) (string, error) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			o := &cliOpts{getenv: func(string) string { return "" }, stdin: strings.NewReader(""), now: time.Now, creds: &cli.Credentials{Keyring: &memKeyring{}}}
			root := newRootWith(o)
			var out, errOut syncBuffer
			root.SetOut(&out)
			root.SetErr(&errOut)
			root.SetArgs(append([]string{"--cli-config", contexts, "--server", "http://" + cfg.Listeners.API.Address, "--token", created.Token, "-o", "json"}, args...))
			err := root.ExecuteContext(t.Context())
			if apiclient.IsKey(err, "generation_pending") && time.Now().Before(deadline) {
				time.Sleep(25 * time.Millisecond)
				continue
			}
			if strings.Contains(out.String(), "test-only-private-credential") || strings.Contains(errOut.String(), "test-only-private-credential") {
				t.Fatal("the CLI printed the credential")
			}
			return out.String(), err
		}
	}
	out, err := run("integrations", "list")
	var list struct {
		Items []api.IntegrationSummary `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 1 || list.Items[0].Name != "records" || list.Items[0].Type != "webhook" {
		t.Fatalf("CLI list: %v\n%s", err, out)
	}
	// Status writes change the revision; the check names the one get read,
	// and a review that a status write superseded is refused before any
	// provider request, so it is read again.
	var summary api.IntegrationSummary
	var checked string
	for deadline := time.Now().Add(30 * time.Second); ; {
		out, err = run("integrations", "get", "records")
		if err != nil || json.Unmarshal([]byte(out), &summary) != nil || summary.UID == "" || summary.Revision == "" {
			t.Fatalf("CLI get: %v\n%s", err, out)
		}
		checked, err = run("integrations", "check", "records", "--uid", summary.UID, "--revision", summary.Revision)
		if err == nil || !apiclient.IsKey(err, api.KeyConflict) || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("CLI check: %v\n%s", err, checked)
	}
	if strings.Contains(checked, "kudurru/owner") {
		t.Fatal("check disclosed the registry marker")
	}
	actions, err := run("integrations", "actions", "records", "--uid", summary.UID, "--revision", summary.Revision)
	t.Logf("CLI actions: %v %s", err, actions)
	if err == nil && strings.Contains(actions, `"name"`) {
		t.Fatal("an external-dns records peer advertised an action", actions)
	}
	posts := peer.postCount()
	if out, err := run("integrations", "act", "records", "purge", "--uid", summary.UID, "--revision", summary.Revision); err == nil {
		t.Fatal("an action the peer does not advertise ran", out)
	} else {
		t.Log("CLI act refusal:", err)
	}
	if peer.postCount() != posts || !webhookRecordsPublished(t, peer) {
		t.Fatal("a CLI read or refused command changed the peer")
	}
}
