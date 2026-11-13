package main

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
)

// awaitMountedBearer waits until a bearer token's GET answers want. Only
// independent reads wait; every command is a single attempt.
func awaitMountedBearer(t *testing.T, base, path, token string, want int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		res, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
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

// TestRunServeSQLiteIAMWritersRevokeAcrossRestart uses the production mount:
// a service account token issued through the API stops working when the
// account is deleted, stays refused after a real restart, and a provider
// object created through the API survives the restart (D396).
func TestRunServeSQLiteIAMWritersRevokeAcrossRestart(t *testing.T) {
	cfg, _ := mountedTestConfig(t)
	cfg.Auth.IdentityProviders = nil
	var revoked, kept string
	var account publicv1.ServiceAccount
	var provider publicv1.IdentityProvider
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
		base := "http://" + cfg.Listeners.API.Address + api.BasePath
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		admin := &http.Client{Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		var status api.AuthStatus
		mountedFactorRequest(t, admin, base, http.MethodGet, "/auth/status", "", nil, http.StatusOK, &status)
		if !status.Capabilities.IAMObjectCommands || !status.Capabilities.LocalUserCommands {
			t.Fatal("complete runtime does not advertise the IAM writers")
		}
		var login api.SessionInfo
		mountedFactorRequest(t, admin, base, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "admin", Password: "mounted-test-password-1"}, http.StatusOK, &login)
		if restart == 0 {
			desired := api.ServiceAccountDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindServiceAccount, Metadata: api.ServiceAccountMetadata{Name: "mounted-robot", Namespace: "apps"}}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts", login.CSRF, desired, http.StatusCreated, &account)
			var first, second api.CreatedToken
			body := api.CreateServiceAccountTokenRequest{UID: string(account.UID), ExpiresIn: "1h"}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts/apps/mounted-robot/tokens", login.CSRF, body, http.StatusCreated, &first)
			awaitMountedBearer(t, base, "/auth/me", first.Token, http.StatusOK)
			mountedFactorRequest(t, admin, base, http.MethodDelete, "/iam/serviceaccounts/apps/mounted-robot", login.CSRF, api.ObjectReview{UID: string(account.UID), ResourceVersion: account.ResourceVersion}, http.StatusNoContent, nil)
			awaitMountedBearer(t, base, "/auth/me", first.Token, http.StatusUnauthorized)
			revoked = first.Token
			// A second account keeps its token across the restart.
			desired.Metadata.Name = "mounted-kept"
			var keptAccount publicv1.ServiceAccount
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts", login.CSRF, desired, http.StatusCreated, &keptAccount)
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/serviceaccounts/apps/mounted-kept/tokens", login.CSRF, api.CreateServiceAccountTokenRequest{UID: string(keptAccount.UID)}, http.StatusCreated, &second)
			kept = second.Token
			awaitMountedBearer(t, base, "/auth/me", kept, http.StatusOK)
			idp := api.IdentityProviderDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindIdentityProvider, Metadata: api.IdentityProviderMetadata{Name: "mounted-partner"},
				Spec: publicv1.IdentityProviderSpec{Issuer: "https://idp.invalid/mounted", ClientID: "kudurru", ClientSecretRef: &publicv1.CredentialReference{Env: "MOUNTED_PARTNER_SECRET"}}}
			mountedFactorRequest(t, admin, base, http.MethodPost, "/iam/identityproviders", login.CSRF, idp, http.StatusCreated, &provider)
		} else {
			awaitMountedBearer(t, base, "/auth/me", revoked, http.StatusUnauthorized)
			awaitMountedBearer(t, base, "/auth/me", kept, http.StatusOK)
			mountedFactorRequest(t, admin, base, http.MethodGet, "/iam/serviceaccounts/apps/mounted-robot", "", nil, http.StatusNotFound, nil)
			var read publicv1.IdentityProvider
			mountedFactorRequest(t, admin, base, http.MethodGet, "/iam/identityproviders/mounted-partner", "", nil, http.StatusOK, &read)
			if read.UID != provider.UID || read.Spec.ClientSecretRef == nil || read.Spec.ClientSecretRef.Env != "MOUNTED_PARTNER_SECRET" {
				t.Fatal("provider object did not survive the restart")
			}
			mountedFactorRequest(t, admin, base, http.MethodDelete, "/iam/identityproviders/mounted-partner", login.CSRF, api.ObjectReview{UID: string(read.UID), ResourceVersion: read.ResourceVersion}, http.StatusNoContent, nil)
		}
		stop()
	}
}
