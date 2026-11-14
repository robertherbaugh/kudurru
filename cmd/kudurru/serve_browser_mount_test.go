package main

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// The actual serve command must mount browser exchange from the complete
// runtime, including both source origins and the real signed-license policy.
func TestRunServeSQLiteCapturedBrowserBothOriginsAndRestart(t *testing.T) {
	sign := trustTestKey(t)
	licenseKey := sign(license.Claims{Plan: license.PlanPersonal, Nodes: 3, Features: []license.Feature{license.FeatureOIDC, license.FeatureRBAC}})
	op := newServingBrowserOP(t)
	cfg, _ := mountedTestConfig(t)
	apiBase := "http://" + cfg.Listeners.API.Address + api.BasePath
	caFile := filepath.Join(t.TempDir(), "op-ca.pem")
	secretFile := filepath.Join(t.TempDir(), "op-client")
	if err := os.WriteFile(caFile, op.CA, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretFile, []byte(op.ClientSecret), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUDURRU_TEST_BROWSER_SECRET", op.ClientSecret)
	cfg.Credentials.AllowedEnvironment = []string{"KUDURRU_TEST_BROWSER_SECRET"}
	cfg.Auth.ProviderResources = config.ProviderResources{AllowedOrigins: []string{op.Server.URL}, LocalOrigins: []string{op.Server.URL}, Algorithms: []string{"EdDSA"}, CAFiles: []string{caFile}}
	cfg.Auth.IdentityProviders = []map[string]any{{"id": "configured", "issuer": op.Issuer("configured"), "clientID": op.ClientID, "clientSecretFile": secretFile, "redirectURL": apiBase + "/auth/oidc/configured/callback"}}
	v, err := validator.New()
	if err != nil {
		t.Fatal(err)
	}
	st, local, _, err := openStore(t.Context(), cfg, v, prometheus.NewRegistry(), slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	for _, object := range []store.Object{
		&public.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "stored"}, Spec: public.IdentityProviderSpec{Issuer: op.Issuer("stored"), ClientID: op.ClientID, ClientSecretRef: &public.CredentialReference{Env: "KUDURRU_TEST_BROWSER_SECRET"}, RedirectURL: apiBase + "/auth/oidc/stored/callback"}},
		&public.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "browser-readers", Namespace: "default"}, Spec: public.RoleBindingSpec{Role: "viewer", Subjects: []public.BindingSubject{{Kind: "Group", Name: "configured:dns"}, {Kind: "Group", Name: "stored:dns"}}}},
	} {
		if _, err := st.Apply(t.Context(), object, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			t.Fatal(err)
		}
	}
	if err := local.Close(); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	var consumedURL, pendingURL string
	var consumedCookie, pendingCookie *http.Cookie
	for restart := range 2 {
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- runServe(ctx, cfg, licenseKey) }()
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
					t.Error("serve browser shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("serve browser shutdown did not join")
			}
		}
		t.Cleanup(stop)
		client := mountedBrowserClient(t)
		awaitMountedBrowserProviders(t, client, apiBase, done)
		// JWT NumericDate is whole seconds. Let real time advance beyond the
		// actual provider activation floor; never fabricate an issuance time.
		boundary := time.Now().Truncate(time.Second).Add(time.Second)
		timer := time.NewTimer(time.Until(boundary))
		select {
		case <-t.Context().Done():
			timer.Stop()
			t.Fatal("browser acceptance canceled")
		case <-timer.C:
		}
		if restart == 0 {
			for _, id := range []string{"configured", "stored"} {
				client = mountedBrowserClient(t)
				callback, preauth := mountedBrowserStart(t, client, op, apiBase, id)
				response, err := client.Get(callback)
				if err != nil {
					t.Fatal("mounted browser callback transport failed")
				}
				_, _ = io.Copy(io.Discard, response.Body)
				_ = response.Body.Close()
				if response.StatusCode != http.StatusFound || response.Header.Get("Location") != "/zones" {
					t.Fatalf("mounted %s browser callback: HTTP %d", id, response.StatusCode)
				}
				var me api.Me
				response, err = client.Get(apiBase + "/auth/me")
				if err != nil {
					t.Fatal("mounted browser session transport failed")
				}
				err = json.NewDecoder(response.Body).Decode(&me)
				_ = response.Body.Close()
				wantID := (oidccore.IdentityKey{Provider: id, Issuer: op.Issuer(id), Subject: "browser-subject"}).String()
				if err != nil || response.StatusCode != http.StatusOK || me.Principal.ID != wantID || me.Principal.Name != id+":alice" {
					t.Fatalf("mounted browser session identity: status=%d decode=%v id=%q name=%q expected-id=%q", response.StatusCode, err, me.Principal.ID, me.Principal.Name, wantID)
				}
				consumedURL, consumedCookie = callback, preauth
			}
			if op.requests.Load() != 2 {
				t.Fatal("successful callbacks did not use exactly their two explicit exchanges")
			}
			pendingURL, pendingCookie = mountedBrowserStart(t, mountedBrowserClient(t), op, apiBase, "configured")
		}
		before := op.requests.Load()
		// Replay a consumed attempt in-process and again after restart. Also
		// retain an unconsumed old activation across restart: provider floors
		// must deny it before any code reaches the upstream server.
		for i, callback := range []string{consumedURL, pendingURL} {
			if restart == 0 && i == 1 {
				continue
			}
			cookie := consumedCookie
			if i == 1 {
				cookie = pendingCookie
			}
			req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, callback, nil)
			if err != nil {
				t.Fatal("invalid private callback fixture")
			}
			req.AddCookie(cookie)
			response, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
			if err != nil {
				t.Fatal("browser replay transport failed")
			}
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()
			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("old browser state returned HTTP %d after restart=%d", response.StatusCode, restart)
			}
		}
		if op.requests.Load() != before {
			t.Fatal("consumed or withdrawn browser state reached upstream exchange")
		}
		stop()
	}
}

func mountedBrowserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func awaitMountedBrowserProviders(t *testing.T, client *http.Client, base string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatal("browser server exited before ready", err)
		default:
		}
		response, err := client.Get(base + "/auth/status")
		if err == nil {
			var status api.AuthStatus
			err = json.NewDecoder(response.Body).Decode(&status)
			_ = response.Body.Close()
			if err == nil && response.StatusCode == http.StatusOK && len(status.Providers) == 2 {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("both original captured provider origins did not become available")
}

func mountedBrowserStart(t *testing.T, client *http.Client, op *servingBrowserOP, base, id string) (string, *http.Cookie) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	for attempt := range 20 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/auth/oidc/"+url.PathEscape(id)+"/start?next=%2Fzones", nil)
		if err != nil {
			t.Fatal("invalid browser start fixture")
		}
		response, err := client.Do(req)
		if err != nil {
			t.Fatal("mounted browser start transport failed")
		}
		if response.StatusCode != http.StatusFound {
			var failure struct {
				Error struct{ Key, Message string } `json:"error"`
			}
			_ = json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&failure)
			_ = response.Body.Close()
			// This exact error is emitted by capture middleware before the
			// start handler, quota or pending-attempt command runs. Only that
			// refusal permits a new start request. Never repeat an admitted
			// attempt, callback, exchange or uncertain transport outcome.
			if response.StatusCode == http.StatusServiceUnavailable && failure.Error.Key == "generation_pending" && attempt < 19 {
				if response.Header.Get("Location") != "" || len(response.Cookies()) != 0 {
					t.Fatal("pre-handler browser refusal returned login state")
				}
				select {
				case <-ctx.Done():
					t.Fatal("browser start publication did not become available")
				case <-time.After(time.Second):
					continue
				}
			}
			t.Fatalf("mounted browser start returned HTTP %d: key=%q message=%q", response.StatusCode, failure.Error.Key, failure.Error.Message)
		}
		_, _ = io.Copy(io.Discard, response.Body)
		_ = response.Body.Close()
		for _, cookie := range response.Cookies() {
			if strings.HasSuffix(cookie.Name, "_oidc") && cookie.HttpOnly && cookie.Value != "" {
				return op.Complete(t, response.Header.Get("Location")), cookie
			}
		}
		t.Fatal("browser start did not issue purpose-separated pending cookie")
	}
	return "", nil
}
