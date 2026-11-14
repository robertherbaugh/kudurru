package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
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

// webhookServeCLI runs one CLI command against a serve listener.
type webhookServeCLI func(args ...string) (string, error)

// startWebhookServe writes definition as a file Integration, starts the real
// serve command with an integrations license, and returns a CLI runner bound
// to a new API token of the bootstrap admin. ready is polled until it holds
// before any CLI command runs. secret must never appear in CLI output.
func startWebhookServe(t *testing.T, cfg *config.Config, definition *public.Integration, secret string, ready func() bool) webhookServeCLI {
	t.Helper()
	definition.TypeMeta = metav1.TypeMeta{APIVersion: public.GroupVersion.String(), Kind: "Integration"}
	raw, err := json.Marshal(definition)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cfg.ObjectsDir, "integration.yaml"), raw, 0600); err != nil {
		t.Fatal(err)
	}
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
				t.Error("webhook mount shutdown failed", err)
			}
		case <-time.After(15 * time.Second):
			t.Error("webhook mount shutdown did not join")
		}
	})
	until := time.Now().Add(20 * time.Second)
	for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
		select {
		case err := <-done:
			t.Fatal("webhook mount ended before readiness", err)
		default:
		}
		if time.Now().After(until) {
			t.Fatal("webhook mount never ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	for deadline := time.Now().Add(60 * time.Second); !ready(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the file webhook Integration never became ready")
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
	mountedFactorRequest(t, c, base, http.MethodPost, "/auth/tokens", login.CSRF, api.CreateTokenRequest{Description: "webhook-mount"}, http.StatusCreated, &created)
	if got := routerContractRequest(t, &http.Client{Timeout: 10 * time.Second}, http.MethodGet, base+"/auth/me", "", created.Token, nil); got.status != http.StatusOK {
		t.Fatal("new token never became usable", got.status, got.key)
	}
	contexts := filepath.Join(t.TempDir(), "contexts.yaml")
	// Only the transient generation_pending answer, given before any command
	// runs, is observed again.
	return func(args ...string) (string, error) {
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
			if strings.Contains(out.String(), secret) || strings.Contains(errOut.String(), secret) {
				t.Fatal("the CLI printed the credential")
			}
			return out.String(), err
		}
	}
}

// reviewed runs one reviewed CLI command (check, actions, act) against the
// revision a CLI get read. Status writes change the revision, and a review a
// status write superseded is refused with a conflict before any provider
// request, so only that answer reads the Integration again and repeats the
// command. It returns the reviewed summary with the command's result.
func (run webhookServeCLI) reviewed(t *testing.T, name string, command ...string) (api.IntegrationSummary, string, error) {
	t.Helper()
	var summary api.IntegrationSummary
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		out, err := run("integrations", "get", name)
		if err != nil || json.Unmarshal([]byte(out), &summary) != nil || summary.UID == "" || summary.Revision == "" {
			t.Fatalf("CLI get: %v\n%s", err, out)
		}
		args := append([]string{"integrations"}, command[0], name)
		args = append(args, command[1:]...)
		out, err = run(append(args, "--uid", summary.UID, "--revision", summary.Revision)...)
		if !apiclient.IsKey(err, api.KeyConflict) || time.Now().After(deadline) {
			return summary, out, err
		}
	}
}

// The real serve command with a file webhook Integration that names domains
// (F60c-Q1, D380): licensed startup, the file source, the elected host and
// the TLS records peer publish the record set with its registry row, and
// the CLI reads, checks and lists actions of the Integration through the
// real API listener. A records peer advertises no action, so the list is
// empty and an act is refused as not implemented, with no request to the
// peer.
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
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "records", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Resync: metav1.Duration{Duration: time.Hour},
		Credentials: map[string]public.CredentialReference{"bearerToken": {File: token}, "ca": {File: peer.ca}},
		Webhook:     &public.WebhookIntegration{Endpoint: peer.server.URL, Domains: []string{"example"}}}}
	seedWebhookRecordsMount(t, cfg)
	// The elected host publishes the file Integration's record set.
	run := startWebhookServe(t, cfg, definition, "test-only-private-credential", func() bool { return webhookRecordsPublished(t, peer) })

	out, err := run("integrations", "list")
	var list struct {
		Items []api.IntegrationSummary `json:"items"`
	}
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list.Items) != 1 || list.Items[0].Name != "records" || list.Items[0].Type != "webhook" {
		t.Fatalf("CLI list: %v\n%s", err, out)
	}
	_, checked, err := run.reviewed(t, "records", "check")
	if err != nil {
		t.Fatalf("CLI check: %v\n%s", err, checked)
	}
	if strings.Contains(checked, "kudurru/owner") {
		t.Fatal("check disclosed the registry marker")
	}
	_, actions, err := run.reviewed(t, "records", "actions")
	var listed api.IntegrationExecutionResult
	if err != nil || json.Unmarshal([]byte(actions), &listed) != nil || len(listed.Actions) != 0 {
		t.Fatalf("CLI actions of a records peer: %v\n%s", err, actions)
	}
	posts := peer.postCount()
	if _, out, err := run.reviewed(t, "records", "act", "purge"); !apiclient.IsKey(err, api.KeyNotImplemented) {
		t.Fatalf("an action the peer does not advertise was not refused as not implemented: %v\n%s", err, out)
	}
	if peer.postCount() != posts || !webhookRecordsPublished(t, peer) {
		t.Fatal("a CLI read or refused command changed the peer")
	}
}

// The successful Tier-B action path through the real serve command (F60c-Q1):
// a file webhook Integration against a TLS actions peer, the CLI lists the
// advertised action and runs it with schema-valid parameters through the
// real API listener; the peer receives exactly one POST, and parameters the
// advertised schema refuses never reach it.
func TestRunServeWebhookActionsFileIntegrationAndCLI(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	token := filepath.Join(t.TempDir(), "actions-token")
	if err := os.WriteFile(token, []byte("test-only-private-credential"), 0600); err != nil {
		t.Fatal(err)
	}
	peer := newWebhookMountPeer(t, false)
	ca := filepath.Join(t.TempDir(), "peer-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: peer.server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	cfg.Credentials.AllowedFiles = []string{token, ca}
	cfg.ObjectsDir = t.TempDir()
	definition := &public.Integration{ObjectMeta: metav1.ObjectMeta{Name: "actions", Namespace: "default"}, Spec: public.IntegrationSpec{Type: "webhook", Resync: metav1.Duration{Duration: 10 * time.Minute},
		Credentials: map[string]public.CredentialReference{"bearerToken": {File: token}, "ca": {File: ca}},
		Webhook:     &public.WebhookIntegration{Endpoint: peer.server.URL + "/provider"}}}
	run := startWebhookServe(t, cfg, definition, "test-only-private-credential", func() bool { return true })

	var actions string
	var err error
	var listed api.IntegrationExecutionResult
	// The lease negotiates after startup; until it has, the instance offers
	// no actions.
	for deadline := time.Now().Add(30 * time.Second); ; time.Sleep(100 * time.Millisecond) {
		_, actions, err = run.reviewed(t, "actions", "actions")
		listed = api.IntegrationExecutionResult{}
		if err == nil && json.Unmarshal([]byte(actions), &listed) == nil && len(listed.Actions) == 1 || time.Now().After(deadline) {
			break
		}
	}
	if err != nil || len(listed.Actions) != 1 || listed.Actions[0].Name != "refresh" {
		t.Fatalf("CLI actions: %v\n%s", err, actions)
	}
	if _, out, err := run.reviewed(t, "actions", "act", "refresh", "--params", `{"confirm":"yes"}`); err == nil || peer.posts.Load() != 0 {
		t.Fatalf("parameters outside the advertised schema were sent: %v\n%s", err, out)
	}
	_, out, err := run.reviewed(t, "actions", "act", "refresh", "--params", `{"confirm":true}`)
	var acted api.IntegrationExecutionResult
	if err != nil || json.Unmarshal([]byte(out), &acted) != nil || !acted.Completed || acted.Uncertain {
		t.Fatalf("CLI act: %v\n%s", err, out)
	}
	if peer.posts.Load() != 1 || peer.unexpected.Load() != 0 {
		t.Fatal("the action reached the peer other than once", peer.posts.Load(), peer.unexpected.Load())
	}
}
