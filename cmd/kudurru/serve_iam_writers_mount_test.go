package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
)

// awaitMountedBearer waits until a bearer token's GET answers want. Only
// independent reads wait; every command is a single attempt.
func awaitMountedBearer(t *testing.T, client *http.Client, base, path, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := client.Do(req)
		if err == nil {
			_ = res.Body.Close()
			if res.StatusCode == want {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s with the issued token never answered %d", path, want)
		}
		select {
		case <-t.Context().Done():
			t.Fatal("canceled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// awaitMountedPublished waits until an independent GET observes the committed
// state: want is 200 (with the exact resourceVersion when one is given), or
// 404 after a delete.
// A later command needs the previous commit's serving generation; a mutation
// sent before it is published may answer 503 generation_pending, and the
// test never retries a mutation to hide that.
func awaitMountedPublished(t *testing.T, client *http.Client, base, path string, want int, resourceVersion string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(req)
		if err == nil {
			var meta struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			decoded := json.NewDecoder(io.LimitReader(res.Body, 64<<10)).Decode(&meta)
			_ = res.Body.Close()
			if res.StatusCode == want && (want != http.StatusOK || resourceVersion == "" || decoded == nil && meta.Metadata.ResourceVersion == resourceVersion) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("GET %s never observed the committed state (%d)", path, want)
		}
		select {
		case <-t.Context().Done():
			t.Fatal("canceled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// TestRunServeSQLiteIAMWritersRevokeAcrossRestart uses the production mount:
// a service account token issued through the API stops working when the
// account is deleted, stays refused after a real restart, and a provider
// object created through the API survives the restart (D396).
func TestRunServeSQLiteIAMWritersRevokeAcrossRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	files, roots := tsigMountedCertificate(t)
	cfg.Listeners.API.TLS.CertFile, cfg.Listeners.API.TLS.KeyFile = files.CertFile, files.KeyFile
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}}
	t.Cleanup(transport.CloseIdleConnections)
	bearerClient := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	var revoked, kept string
	var account publicv1.ServiceAccount
	var provider publicv1.IdentityProvider
	var oldUserCookie *http.Cookie
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
					t.Error("IAM mount shutdown failed", err)
				}
			case <-time.After(15 * time.Second):
				t.Error("IAM mount shutdown did not join")
			}
		}
		t.Cleanup(stop)
		deadline := time.Now().Add(20 * time.Second)
		for offlineHTTPStatus(cfg.Listeners.Ops.Address, "/healthz/ready") != http.StatusOK {
			select {
			case err := <-done:
				t.Fatal("IAM mount startup ended", err)
			case <-time.After(20 * time.Millisecond):
			}
			if time.Now().After(deadline) {
				t.Fatal("IAM mount never became ready")
			}
		}
		base := "https://" + cfg.Listeners.API.Address + api.BasePath
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		admin := &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var status api.AuthStatus
		mountedFactorRequest(t, admin, base, http.MethodGet, "/auth/status", "", nil, http.StatusOK, &status)
		if !status.Capabilities.IAMObjectCommands || !status.Capabilities.LocalUserCommands {
			t.Fatal("complete runtime does not advertise the IAM writers")
		}
		var login api.SessionInfo
		mountedFactorRequest(t, admin, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		// The session enrolled by the login is a commit of its own.
		awaitMountedPublished(t, admin, base, "/auth/me", http.StatusOK, "")
		if restart == 0 {
			var user api.IAMUser
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/users", login.CSRF, api.CreateLocalUserRequest{Name: "mounted-person", Password: "temporary-password-1"}, http.StatusCreated, &user)
			awaitMountedUser(t, admin, base, user.Name, user.ResourceVersion, true)
			// An explicit cookie lets the proof replay it after revocation even
			// though a browser jar would discard a cleared cookie.
			client := authReloadTLSClient{client: bearerClient, base: "https://" + cfg.Listeners.API.Address}
			userLogin := client.call(t, "POST", "/auth/login", `{"username":"mounted-person","password":"temporary-password-1"}`, "", nil, "")
			expectServingHTTPStatus(t, userLogin, http.StatusOK)
			oldUserCookie = logoutCookieFrom(t, userLogin, "kudurru_session")
			user = awaitMountedUser(t, admin, base, user.Name, "", true)
			disabled := true
			mountedFactorRequest(t, admin, base, http.MethodPatch, "/iam/users/mounted-person", login.CSRF, api.UpdateLocalUserRequest{UID: user.UID, ResourceVersion: user.ResourceVersion, Disabled: &disabled}, http.StatusOK, &user)
			awaitMountedUser(t, admin, base, user.Name, user.ResourceVersion, true)
			expectServingHTTPStatus(t, client.call(t, "GET", "/auth/me", "", "", oldUserCookie, ""), http.StatusUnauthorized)
			mountedFactorRequest(t, admin, base, http.MethodDelete, "/iam/users/mounted-person", login.CSRF, api.ObjectReview{UID: user.UID, ResourceVersion: user.ResourceVersion}, http.StatusNoContent, nil)
			awaitMountedUser(t, admin, base, user.Name, "", false)
			desired := api.ServiceAccountDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindServiceAccount, Metadata: api.ServiceAccountMetadata{Name: "mounted-robot", Namespace: "apps"}}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts", login.CSRF, desired, http.StatusCreated, &account)
			awaitMountedPublished(t, admin, base, "/iam/serviceaccounts/apps/mounted-robot", http.StatusOK, account.ResourceVersion)
			var first, second api.CreatedToken
			body := api.CreateServiceAccountTokenRequest{UID: string(account.UID), ExpiresIn: "1h"}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts/apps/mounted-robot/tokens", login.CSRF, body, http.StatusCreated, &first)
			awaitMountedBearer(t, bearerClient, base, "/auth/me", first.Token, http.StatusOK)
			desired.Spec.Disabled = true
			mountedFactorRequest(t, admin, base, http.MethodPut, "/iam/serviceaccounts/apps/mounted-robot", login.CSRF, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&account), ServiceAccount: desired}, http.StatusOK, &account)
			awaitMountedPublished(t, admin, base, "/iam/serviceaccounts/apps/mounted-robot", http.StatusOK, account.ResourceVersion)
			awaitMountedBearer(t, bearerClient, base, "/auth/me", first.Token, http.StatusUnauthorized)
			mountedFactorRequest(t, admin, base, http.MethodDelete, "/iam/serviceaccounts/apps/mounted-robot", login.CSRF, api.ObjectReview{UID: string(account.UID), ResourceVersion: account.ResourceVersion}, http.StatusNoContent, nil)
			awaitMountedPublished(t, admin, base, "/iam/serviceaccounts/apps/mounted-robot", http.StatusNotFound, "")
			awaitMountedBearer(t, bearerClient, base, "/auth/me", first.Token, http.StatusUnauthorized)
			revoked = first.Token
			// A second account keeps its token across the restart.
			desired.Metadata.Name = "mounted-kept"
			desired.Spec.Disabled = false
			var keptAccount publicv1.ServiceAccount
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts", login.CSRF, desired, http.StatusCreated, &keptAccount)
			awaitMountedPublished(t, admin, base, "/iam/serviceaccounts/apps/mounted-kept", http.StatusOK, keptAccount.ResourceVersion)
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts/apps/mounted-kept/tokens", login.CSRF, api.CreateServiceAccountTokenRequest{UID: string(keptAccount.UID)}, http.StatusCreated, &second)
			kept = second.Token
			awaitMountedBearer(t, bearerClient, base, "/auth/me", kept, http.StatusOK)
			idp := api.IdentityProviderDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindIdentityProvider, Metadata: api.IdentityProviderMetadata{Name: "mounted-partner"},
				Spec: publicv1.IdentityProviderSpec{Issuer: "https://idp.invalid/mounted", ClientID: "kudurru", ClientSecretRef: &publicv1.CredentialReference{Env: "MOUNTED_PARTNER_SECRET"}}}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/identityproviders", login.CSRF, idp, http.StatusCreated, &provider)
		} else {
			client := authReloadTLSClient{client: bearerClient, base: "https://" + cfg.Listeners.API.Address}
			expectServingHTTPStatus(t, client.call(t, "GET", "/auth/me", "", "", oldUserCookie, ""), http.StatusUnauthorized)
			awaitMountedUser(t, admin, base, "mounted-person", "", false)
			awaitMountedBearer(t, bearerClient, base, "/auth/me", revoked, http.StatusUnauthorized)
			awaitMountedBearer(t, bearerClient, base, "/auth/me", kept, http.StatusOK)
			mountedFactorRequest(t, admin, base, http.MethodGet, "/iam/serviceaccounts/apps/mounted-robot", "", nil, http.StatusNotFound, nil)
			var read publicv1.IdentityProvider
			mountedFactorRequest(t, admin, base, http.MethodGet, "/iam/identityproviders/mounted-partner", "", nil, http.StatusOK, &read)
			desired := identityProviderManifest("mounted-partner")
			desired.Spec = read.Spec
			desired.Spec.DisplayName = "Updated partner"
			mountedFactorRequest(t, admin, base, http.MethodPut, "/iam/identityproviders/mounted-partner", login.CSRF, api.ReplaceIdentityProviderCommand{ObjectReview: objectReview(&read), IdentityProvider: desired}, http.StatusOK, &read)
			awaitMountedPublished(t, admin, base, "/iam/identityproviders/mounted-partner", http.StatusOK, read.ResourceVersion)
			if read.UID != provider.UID || read.Spec.ClientSecretRef == nil || read.Spec.ClientSecretRef.Env != "MOUNTED_PARTNER_SECRET" {
				t.Fatal("provider object did not survive the restart")
			}
			mountedFactorRequest(t, admin, base, http.MethodDelete, "/iam/identityproviders/mounted-partner", login.CSRF, api.ObjectReview{UID: string(read.UID), ResourceVersion: read.ResourceVersion}, http.StatusNoContent, nil)
		}
		stop()
	}
}

// Observe the exact user revision through the safe inventory projection.
func awaitMountedUser(t *testing.T, client *http.Client, base, name, revision string, present bool) api.IAMUser {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/iam/users", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := client.Do(request)
		if err == nil {
			var page api.AuthList[api.IAMUser]
			decoded := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&page)
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK && decoded == nil {
				found := false
				for _, user := range page.Items {
					if user.Name == name {
						found = true
						if present && (user.ResourceVersion == revision || revision == "" && user.LastLoginAt != nil) {
							return user
						}
					}
				}
				if !present && !found {
					return api.IAMUser{}
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatal("user inventory never observed the committed revision")
		}
		select {
		case <-t.Context().Done():
			t.Fatal("canceled")
		case <-time.After(20 * time.Millisecond):
		}
	}
}
