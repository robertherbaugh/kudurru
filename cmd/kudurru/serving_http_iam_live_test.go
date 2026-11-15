package main

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Opt-in external acceptance. The operator provisions a disposable confidential
// Authentik client with client_credentials and openid/profile scopes. The file
// contains issuer, clientID and clientSecret, and is never logged. The fixture
// obtains NEW signed ID tokens from its public, normally trusted TLS endpoint;
// it does not import a signing key or weaken TLS/JWT verification.
func TestServingHTTPIAMLiveAuthentikRevocation(t *testing.T) {
	path := os.Getenv("KUDURRU_ACCEPTANCE_OIDC_FILE")
	if path == "" {
		t.Skip("set KUDURRU_ACCEPTANCE_OIDC_FILE for disposable external OIDC acceptance")
	}
	var external struct{ Issuer, ClientID, ClientSecret string }
	raw, err := os.ReadFile(path) // #nosec G304 G703 -- Opt-in test reads the operator-selected private credential file.
	if err != nil || json.Unmarshal(raw, &external) != nil || external.ClientID == "" || external.ClientSecret == "" {
		t.Fatal("invalid private acceptance credentials")
	}
	issuer, err := url.Parse(external.Issuer)
	if err != nil || issuer.Scheme != "https" || issuer.Host == "" {
		t.Fatal("external acceptance requires HTTPS issuer")
	}
	origin := issuer.Scheme + "://" + issuer.Host
	secretFile := filepath.Join(t.TempDir(), "external-client-secret")
	if os.WriteFile(secretFile, []byte(external.ClientSecret), 0o600) != nil {
		t.Fatal("private credential file")
	}
	f := newServingBrowserFixturePolicy(t, nil, func(cfg *config.Config) {
		cfg.Credentials.AllowedFiles = append(cfg.Credentials.AllowedFiles, secretFile)
		cfg.Auth.ProviderResources.AllowedOrigins = append(cfg.Auth.ProviderResources.AllowedOrigins, origin)
		cfg.Auth.ProviderResources.SystemTrust = true
		cfg.Auth.ProviderResources.Algorithms = append(cfg.Auth.ProviderResources.Algorithms, "RS256")
	})
	admin, csrf := externalAdminLogin(t, f)
	localUsersFresh(t, f.servingHTTPFixture)
	c := iamAcceptanceHTTP(t, f.s)
	desired := identityProviderManifest("acceptance")
	desired.Spec.Issuer = external.Issuer
	desired.Spec.ClientID = external.ClientID
	desired.Spec.ClientSecretRef = &publicv1.CredentialReference{File: secretFile}
	// This disposable application's machine principal receives viewer only.
	desired.Spec.Provisioning.DefaultRole = "viewer"
	desired.Spec.Provisioning.AutoProvision = "always"
	provider := identityProviderResponse(t, c.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, desired), "", admin, csrf), http.StatusCreated)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		info, e := v.ProviderInfo("acceptance")
		return e == nil && info.Availability == servingauth.ProviderReady
	})
	follower := iamAcceptanceFollower(t, f, "acceptance")
	fc := iamAcceptanceHTTP(t, follower.s)
	awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > provider.CreationTimestamp.Unix() })
	// Authentik uses one public token endpoint for its application issuers.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, origin+"/application/o/token/", strings.NewReader(url.Values{"grant_type": {"client_credentials"}, "scope": {"openid profile email"}}.Encode()))
	if err != nil {
		t.Fatal("external token request")
	}
	request.SetBasicAuth(external.ClientID, external.ClientSecret)
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		t.Fatal("external token TLS transport")
	}
	var minted struct {
		IDToken string `json:"id_token"`
	}
	decoded := json.NewDecoder(response.Body).Decode(&minted)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || decoded != nil || minted.IDToken == "" {
		t.Fatal("external provider did not issue an ID token", response.StatusCode)
	}
	expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", minted.IDToken, nil, ""), http.StatusOK)

	expectServingHTTPStatus(t, fc.call(t, "GET", "/auth/me", "", minted.IDToken, nil, ""), http.StatusOK)
	expectServingHTTPStatus(t, c.call(t, "DELETE", "/iam/identityproviders/acceptance", roleBindingBody(t, objectReview(&provider)), "", admin, csrf), http.StatusNoContent)
	localUsersFresh(t, f.servingHTTPFixture)
	expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", minted.IDToken, nil, ""), http.StatusUnauthorized)
	installCtx := captureServingReplica(t, follower.store)
	image, err := f.s.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.store.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = follower.store.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: follower.store.Authority().Installed}); err != nil {
		t.Fatal(err)
	}
	awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool { return v.Base().Same(image.Generation()) })
	expectServingHTTPStatus(t, fc.call(t, "GET", "/auth/me", "", minted.IDToken, nil, ""), http.StatusUnauthorized)
	follower.reopen(t.Context(), t)
	runServingFixture(t, follower.s)
	restarted := iamAcceptanceHTTP(t, follower.s)
	awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool { return v.Base().Same(image.Generation()) })
	expectServingHTTPStatus(t, restarted.call(t, "GET", "/auth/me", "", minted.IDToken, nil, ""), http.StatusUnauthorized)
	expectServingHTTPStatus(t, restarted.call(t, "GET", "/auth/me", "", "", admin, ""), http.StatusOK)
	t.Logf("live issuer=%s: original signed ID token accepted on primary/follower; refused on both after API provider delete and follower restart; unrelated local session survived", external.Issuer)
}
