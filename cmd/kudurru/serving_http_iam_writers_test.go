package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func serviceAccountManifest(name string) api.ServiceAccountDesired {
	return api.ServiceAccountDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindServiceAccount, Metadata: api.ServiceAccountMetadata{Name: name, Namespace: "apps"}, Spec: api.ServiceAccountFields{Description: "automation"}}
}

func serviceAccountResponse(t *testing.T, w *httptest.ResponseRecorder, status int) publicv1.ServiceAccount {
	t.Helper()
	expectServingHTTPStatus(t, w, status)
	var out publicv1.ServiceAccount
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UID == "" || out.ResourceVersion == "" || store.ManagedBy(&out) != store.ManagerAPI || out.Kind != publicv1.KindServiceAccount {
		t.Fatal("missing reviewed ServiceAccount metadata")
	}
	return out
}

func objectReview(obj metav1.Object) api.ObjectReview {
	return api.ObjectReview{UID: string(obj.GetUID()), ResourceVersion: obj.GetResourceVersion()}
}

func issueServiceAccountToken(t *testing.T, f *servingHTTPFixture, sa publicv1.ServiceAccount, status int) api.CreatedToken {
	t.Helper()
	w := f.call(t, "POST", "/iam/serviceaccounts/apps/"+sa.Name+"/tokens", roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(sa.UID), Description: "ci", ExpiresIn: "1h"}), f.token, nil, "")
	expectServingHTTPStatus(t, w, status)
	var out api.CreatedToken
	if status == http.StatusCreated {
		if json.Unmarshal(w.Body.Bytes(), &out) != nil || !strings.HasPrefix(out.Token, "kdr_") || out.Owner.Kind != internalv1.OwnerServiceAccount || out.Owner.UID != string(sa.UID) || out.ExpiresAt == nil {
			t.Fatal("service account token answer")
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("issued token answer is cacheable")
		}
	}
	return out
}

// ownedTokens counts the stored tokens of one exact account incarnation.
func ownedTokens(t *testing.T, st *sqlite.Store, uid string) int {
	t.Helper()
	list, err := st.List(t.Context(), "APIToken", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, o := range list.Items {
		if o.(*internalv1.APIToken).Spec.Owner.UID == uid {
			n++
		}
	}
	return n
}

// secretNeverStored fails when a token secret appears in the stored image or
// the audit trail after its one-time answer.
func secretNeverStored(t *testing.T, st *sqlite.Store, secret string) {
	t.Helper()
	image := roleBindingImage(t, st)
	raw, err := json.Marshal(image.Payload)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := st.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	audit, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), secret) || strings.Contains(string(audit), secret) {
		t.Fatal("issued token secret was stored or audited")
	}
}

func TestServingHTTPIAMServiceAccountsReviewedLifecycle(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	in := serviceAccountManifest("ci")
	created := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, in), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	read := serviceAccountResponse(t, f.call(t, "GET", "/iam/serviceaccounts/apps/ci", "", f.token, nil, ""), http.StatusOK)
	if read.UID != created.UID || read.ResourceVersion != created.ResourceVersion || read.Spec.Description != "automation" {
		t.Fatal("read changed review")
	}
	before := roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, in), f.token, nil, ""), http.StatusConflict)
	roleBindingUnchanged(t, f.s.opts.Store, before)

	// A binding names the account; this fixture holds no management
	// entitlement, so the checks below use authentication (GET /auth/me).
	roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, api.RoleBindingDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindRoleBinding, Metadata: api.RoleBindingMetadata{Name: "ci-view", Namespace: "apps"}, Spec: api.RoleBindingFields{Role: "viewer", Subjects: []api.BindingSubject{{Kind: "ServiceAccount", Name: "ci", Namespace: "apps"}}}}), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	before = roleBindingImage(t, f.s.opts.Store)
	stale := created
	stale.UID = "other"
	issueServiceAccountToken(t, f, stale, http.StatusConflict)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	token := issueServiceAccountToken(t, f, created, http.StatusCreated)
	localUsersFresh(t, f)
	secretNeverStored(t, f.s.opts.Store, token.Token)
	// A token never outlives the API token that issued it: without expiresIn
	// (maxLifetime 0) and with a longer one, the caller's expiry is the bound.
	for _, life := range []string{"", "48h"} {
		w := f.call(t, "POST", "/iam/serviceaccounts/apps/ci/tokens", roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(created.UID), ExpiresIn: life}), f.token, nil, "")
		expectServingHTTPStatus(t, w, http.StatusCreated)
		var bounded api.CreatedToken
		if json.Unmarshal(w.Body.Bytes(), &bounded) != nil || bounded.ExpiresAt == nil || !bounded.ExpiresAt.Equal(f.expires) {
			t.Fatalf("token issued with expiresIn %q outlives its issuer: %v, want %v", life, bounded.ExpiresAt, f.expires)
		}
		localUsersFresh(t, f)
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token.Token, nil, ""), http.StatusOK)
	// The account's own token cannot administer IAM.
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("other")), token.Token, nil, ""), http.StatusForbidden)
	listed := f.call(t, "GET", "/auth/tokens?all=true", "", f.token, nil, "")
	expectServingHTTPStatus(t, listed, http.StatusOK)
	if !strings.Contains(listed.Body.String(), token.ID) || strings.Contains(listed.Body.String(), token.Token) {
		t.Fatal("token inventory lacks the token or shows its secret")
	}

	// Stale reviews change nothing.
	for _, review := range []api.ObjectReview{{UID: "other", ResourceVersion: created.ResourceVersion}, {UID: string(created.UID), ResourceVersion: "stale"}} {
		before = roleBindingImage(t, f.s.opts.Store)
		expectServingHTTPStatus(t, f.call(t, "PUT", "/iam/serviceaccounts/apps/ci", roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: review, ServiceAccount: in}), f.token, nil, ""), http.StatusConflict)
		expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/serviceaccounts/apps/ci", roleBindingBody(t, review), f.token, nil, ""), http.StatusConflict)
		roleBindingUnchanged(t, f.s.opts.Store, before)
	}

	// Disabling raises the account floor and deletes its tokens atomically.
	in.Spec.Disabled = true
	disabled := serviceAccountResponse(t, f.call(t, "PUT", "/iam/serviceaccounts/apps/ci", roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&created), ServiceAccount: in}), f.token, nil, ""), http.StatusOK)
	if disabled.UID != created.UID || disabled.ResourceVersion == created.ResourceVersion || !disabled.Spec.Disabled {
		t.Fatal("replacement lost identity or CAS")
	}
	localUsersFresh(t, f)
	if ownedTokens(t, f.s.opts.Store, string(created.UID)) != 0 {
		t.Fatal("disable kept the account's tokens")
	}
	floor := false
	for _, a := range localUsersState(t, f.s.opts.Store).AccountFloors {
		floor = floor || a.Account.Kind == "ServiceAccount" && a.Account.UID == string(created.UID)
	}
	if !floor {
		t.Fatal("disable omitted the durable account floor")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token.Token, nil, ""), http.StatusUnauthorized)
	before = roleBindingImage(t, f.s.opts.Store)
	issueServiceAccountToken(t, f, disabled, http.StatusConflict)
	roleBindingUnchanged(t, f.s.opts.Store, before)

	// Enabling restores nothing.
	in.Spec.Disabled = false
	enabled := serviceAccountResponse(t, f.call(t, "PUT", "/iam/serviceaccounts/apps/ci", roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&disabled), ServiceAccount: in}), f.token, nil, ""), http.StatusOK)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token.Token, nil, ""), http.StatusUnauthorized)
	second := issueServiceAccountToken(t, f, enabled, http.StatusCreated)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", second.Token, nil, ""), http.StatusOK)

	// Delete ends the account and its tokens; a recreated name inherits nothing.
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/serviceaccounts/apps/ci", roleBindingBody(t, objectReview(&enabled)), f.token, nil, ""), http.StatusNoContent)
	localUsersFresh(t, f)
	if ownedTokens(t, f.s.opts.Store, string(created.UID)) != 0 {
		t.Fatal("delete kept the account's tokens")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/serviceaccounts/apps/ci", "", f.token, nil, ""), http.StatusNotFound)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", second.Token, nil, ""), http.StatusUnauthorized)
	recreated := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, in), f.token, nil, ""), http.StatusCreated)
	if recreated.UID == created.UID {
		t.Fatal("recreate reused the incarnation")
	}
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", second.Token, nil, ""), http.StatusUnauthorized)
	secretNeverStored(t, f.s.opts.Store, second.Token)
}

func TestServingHTTPIAMServiceAccountsStrictInputOwnershipAndPermission(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	valid := roleBindingBody(t, serviceAccountManifest("strict"))
	before := roleBindingImage(t, f.s.opts.Store)
	for _, body := range []string{
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"ServiceAccount","metadata":{"name":"strict","namespace":"apps"},"spec":{"token":{"secretName":"x"}}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"ServiceAccount","kind":"ServiceAccount","metadata":{"name":"strict","namespace":"apps"},"spec":{}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"ServiceAccount","metadata":{"name":"strict","namespace":"apps","uid":"chosen"},"spec":{}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"ServiceAccount","metadata":{"name":"strict","namespace":"apps","annotations":{"kudurru.io/managed-by":"api"}},"spec":{}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"RoleBinding","metadata":{"name":"strict","namespace":"apps"},"spec":{}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"ServiceAccount","metadata":{"name":"Not_Valid","namespace":"apps"},"spec":{}}`,
	} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", body, f.token, nil, ""), http.StatusBadRequest)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts?dryRun=1", valid, f.token, nil, ""), http.StatusBadRequest)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	cookie, info := f.login(t)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", valid, "", cookie, "wrong"), http.StatusForbidden)
	w := f.call(t, "POST", "/auth/tokens", `{"scopes":["viewer"]}`, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, http.StatusCreated)
	var viewer api.CreatedToken
	if json.Unmarshal(w.Body.Bytes(), &viewer) != nil {
		t.Fatal("token response")
	}
	localUsersFresh(t, f)
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", valid, viewer.Token, nil, ""), http.StatusForbidden)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/identityproviders", "{}", viewer.Token, nil, ""), http.StatusForbidden)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/users/someone", `{"uid":"x","resourceVersion":"1"}`, viewer.Token, nil, ""), http.StatusForbidden)
	roleBindingUnchanged(t, f.s.opts.Store, before)

	// Accounts of another manager and accounts with bootstrap authority are
	// never changed or deleted here.
	var system, bootstrap publicv1.ServiceAccount
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		out, err := tx.Apply(ctx, &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "system-owned", Namespace: "apps"}}, store.ApplyOptions{Manager: store.ManagerSystem})
		if err != nil {
			return err
		}
		system = *out.(*publicv1.ServiceAccount)
		out, err = tx.Apply(ctx, &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "bootstrap-like", Namespace: "apps"}}, store.ApplyOptions{Manager: store.ManagerAPI})
		if err != nil {
			return err
		}
		bootstrap = *out.(*publicv1.ServiceAccount)
		_, err = tx.Apply(ctx, &internalv1.PrincipalState{ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(serviceAccountOwner(&bootstrap))}, Spec: internalv1.PrincipalStateSpec{Owner: serviceAccountOwner(&bootstrap), BuiltinAdmin: true}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	listed := f.call(t, "GET", "/iam/serviceaccounts", "", f.token, nil, "")
	expectServingHTTPStatus(t, listed, http.StatusOK)
	var accounts api.AuthList[api.ServiceAccountInfo]
	if json.Unmarshal(listed.Body.Bytes(), &accounts) != nil {
		t.Fatal("invalid service account inventory")
	}
	managers := map[string]string{}
	for _, sa := range accounts.Items {
		managers[sa.Namespace+"/"+sa.Name] = sa.ManagedBy
	}
	if managers["apps/system-owned"] != store.ManagerSystem || managers["apps/bootstrap-like"] != store.ManagerAPI {
		t.Fatalf("service account inventory managers %v", managers)
	}
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/serviceaccounts/apps/system-owned", roleBindingBody(t, objectReview(&system)), f.token, nil, ""), http.StatusConflict)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/serviceaccounts/apps/bootstrap-like", roleBindingBody(t, objectReview(&bootstrap)), f.token, nil, ""), http.StatusForbidden)
	// No token is minted for them either: a bootstrap-authority token would
	// carry admin with no binding to revoke.
	issueServiceAccountToken(t, f, system, http.StatusConflict)
	issueServiceAccountToken(t, f, bootstrap, http.StatusForbidden)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	f.stop()
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts", valid, f.token, nil, ""), http.StatusServiceUnavailable)
	roleBindingUnchanged(t, f.s.opts.Store, before)
}

func TestServingHTTPIAMLocalUserUpdateAndDelete(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	created := localUsersCreate(t, f, "managed-person")
	cookie, info := localUsersLogin(t, f, "managed-person", "temporary-password", "")
	w := f.call(t, "POST", "/auth/password", factorBody(t, api.PasswordChangeRequest{CurrentPassword: "temporary-password", NewPassword: "chosen-password-1"}), "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, http.StatusOK)
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || len(w.Result().Cookies()) != 1 {
		t.Fatal("password change answer")
	}
	cookie = w.Result().Cookies()[0]
	localUsersFresh(t, f)
	current := localUsersRead(t, f.s.opts.Store, "managed-person")
	patch := func(body any, status int) api.IAMUser {
		t.Helper()
		w := f.call(t, "PATCH", "/iam/users/managed-person", factorBody(t, body), f.token, nil, "")
		expectServingHTTPStatus(t, w, status)
		var out api.IAMUser
		if status == http.StatusOK {
			if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UID != created.UID || out.ResourceVersion == "" {
				t.Fatal("update answer")
			}
			localUsersFresh(t, f)
		}
		return out
	}
	before := roleBindingImage(t, f.s.opts.Store)
	patch(map[string]any{"uid": created.UID, "resourceVersion": created.ResourceVersion, "displayName": "x"}, http.StatusConflict)
	patch(map[string]any{"uid": created.UID, "resourceVersion": current.ResourceVersion}, http.StatusBadRequest)
	patch(map[string]any{"uid": created.UID, "resourceVersion": current.ResourceVersion, "groups": []string{"corp:admins"}}, http.StatusBadRequest)
	patch(map[string]any{"uid": created.UID, "resourceVersion": current.ResourceVersion, "groups": []string{"a", "a"}}, http.StatusBadRequest)
	patch(map[string]any{"uid": created.UID, "resourceVersion": current.ResourceVersion, "password": "x"}, http.StatusBadRequest)
	roleBindingUnchanged(t, f.s.opts.Store, before)

	// A display-name change keeps the session; a group change ends it and the
	// personal tokens of the account.
	renamed := patch(map[string]any{"uid": created.UID, "resourceVersion": current.ResourceVersion, "displayName": "Renamed"}, http.StatusOK)
	if renamed.DisplayName != "Renamed" {
		t.Fatal("display name not changed")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), http.StatusOK)
	personalWire, _ := localUsersAddToken(t, f, internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: "managed-person", ID: "local:managed-person", Provider: "local", UID: created.UID}, store.ManagerSystem)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", personalWire, nil, ""), http.StatusOK)
	localUsersFresh(t, f)
	grouped := patch(map[string]any{"uid": renamed.UID, "resourceVersion": renamed.ResourceVersion, "groups": []string{"operators"}}, http.StatusOK)
	if len(grouped.Groups) != 1 || grouped.Groups[0] != "operators" || grouped.MustChangePassword {
		t.Fatal("group change projection")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), http.StatusUnauthorized)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", personalWire, nil, ""), http.StatusUnauthorized)
	if ownedTokens(t, f.s.opts.Store, created.UID) != 0 {
		t.Fatal("group change kept personal tokens")
	}
	disabled := patch(map[string]any{"uid": grouped.UID, "resourceVersion": grouped.ResourceVersion, "disabled": true}, http.StatusOK)
	if !disabled.Disabled {
		t.Fatal("disable projection")
	}
	f.h.factorLimit.Reset("managed-person")
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", factorBody(t, api.LoginRequest{Username: "managed-person", Password: "chosen-password-1"}), "", nil, ""), http.StatusUnauthorized)

	// The built-in account is excluded.
	root := localUsersRead(t, f.s.opts.Store, "root-command")
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/users/root-command", factorBody(t, api.ObjectReview{UID: string(root.UID), ResourceVersion: root.ResourceVersion}), f.token, nil, ""), http.StatusForbidden)
	roleBindingUnchanged(t, f.s.opts.Store, before)

	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/users/managed-person", factorBody(t, api.ObjectReview{UID: disabled.UID, ResourceVersion: grouped.ResourceVersion}), f.token, nil, ""), http.StatusConflict)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/users/managed-person", factorBody(t, api.ObjectReview{UID: disabled.UID, ResourceVersion: disabled.ResourceVersion}), f.token, nil, ""), http.StatusNoContent)
	localUsersFresh(t, f)
	floor := false
	for _, a := range localUsersState(t, f.s.opts.Store).AccountFloors {
		floor = floor || a.Account.Kind == "LocalUser" && a.Account.UID == created.UID
	}
	if !floor {
		t.Fatal("delete omitted the account floor")
	}
	again := localUsersCreate(t, f, "managed-person")
	if again.UID == created.UID {
		t.Fatal("recreated account reused the incarnation")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", personalWire, nil, ""), http.StatusUnauthorized)
}

func identityProviderManifest(name string) api.IdentityProviderDesired {
	return api.IdentityProviderDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindIdentityProvider, Metadata: api.IdentityProviderMetadata{Name: name},
		Spec: publicv1.IdentityProviderSpec{DisplayName: "Partner", Issuer: "https://idp.invalid/" + name, ClientID: "kudurru", ClientSecretRef: &publicv1.CredentialReference{Env: "PARTNER_CLIENT_SECRET"}, RedirectURL: "https://kudurru.test/api/v1/auth/oidc/" + name + "/callback"}}
}

func identityProviderResponse(t *testing.T, w *httptest.ResponseRecorder, status int) publicv1.IdentityProvider {
	t.Helper()
	expectServingHTTPStatus(t, w, status)
	var out publicv1.IdentityProvider
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UID == "" || out.ResourceVersion == "" || store.ManagedBy(&out) != store.ManagerAPI || out.Spec.ClientSecretRef == nil {
		t.Fatal("missing reviewed IdentityProvider metadata")
	}
	return out
}

func TestServingHTTPIAMIdentityProvidersReviewedLifecycle(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	in := identityProviderManifest("partner")
	for _, body := range []string{
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"IdentityProvider","metadata":{"name":"partner"},"spec":{"issuer":"https://idp.invalid","clientID":"x","clientSecret":"plain"}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"IdentityProvider","metadata":{"name":"partner"},"spec":{"issuer":"https://idp.invalid","clientID":"x"}}`,
		`{"apiVersion":"kudurru.io/v1alpha1","kind":"IdentityProvider","metadata":{"name":"partner","namespace":"apps"},"spec":{"issuer":"https://idp.invalid","clientID":"x","clientSecretRef":{"env":"X"}}}`,
	} {
		w := f.call(t, "POST", "/iam/identityproviders", body, f.token, nil, "")
		expectServingHTTPStatus(t, w, http.StatusBadRequest)
		if strings.Contains(w.Body.String(), "plain") {
			t.Fatal("refusal echoed a secret value")
		}
	}
	created := identityProviderResponse(t, f.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, in), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	read := identityProviderResponse(t, f.call(t, "GET", "/iam/identityproviders/partner", "", f.token, nil, ""), http.StatusOK)
	if read.UID != created.UID || read.ResourceVersion != created.ResourceVersion {
		t.Fatal("read changed review")
	}
	before := roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, in), f.token, nil, ""), http.StatusConflict)
	expectServingHTTPStatus(t, f.call(t, "PUT", "/iam/identityproviders/partner", roleBindingBody(t, api.ReplaceIdentityProviderCommand{ObjectReview: api.ObjectReview{UID: string(created.UID), ResourceVersion: "stale"}, IdentityProvider: in}), f.token, nil, ""), http.StatusConflict)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	// Other IAM commands keep working after the provider set changed.
	serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("after-idp")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)

	providerFloor := func(uid string) bool {
		for _, p := range localUsersState(t, f.s.opts.Store).ProviderFloors {
			if p.Provider == "partner" && p.UID == uid {
				return true
			}
		}
		return false
	}
	in.Spec.DisplayName = "Partner corp"
	replaced := identityProviderResponse(t, f.call(t, "PUT", "/iam/identityproviders/partner", roleBindingBody(t, api.ReplaceIdentityProviderCommand{ObjectReview: objectReview(&created), IdentityProvider: in}), f.token, nil, ""), http.StatusOK)
	if replaced.UID != created.UID || replaced.Spec.DisplayName != "Partner corp" {
		t.Fatal("replacement")
	}
	localUsersFresh(t, f)
	if !providerFloor(string(created.UID)) {
		t.Fatal("changed provider omitted its revocation floor")
	}
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/identityproviders/partner", roleBindingBody(t, objectReview(&replaced)), f.token, nil, ""), http.StatusNoContent)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/identityproviders/partner", "", f.token, nil, ""), http.StatusNotFound)
	recreated := identityProviderResponse(t, f.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, in), f.token, nil, ""), http.StatusCreated)
	if recreated.UID == created.UID || !providerFloor(string(created.UID)) {
		t.Fatal("recreate reused the incarnation or erased the floor")
	}
}
