package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func externalAdminLogin(t *testing.T, f *servingBrowserFixture) (*http.Cookie, string) {
	t.Helper()
	hash, err := users.NewHasher(1).Hash(t.Context(), "external-admin-password")
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "external-admin"}, Spec: internalv1.LocalUserSpec{BuiltinAdmin: true, PasswordHash: hash}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		list, err := v.Objects("LocalUser")
		return err == nil && len(list) > 0
	})
	w := f.call(t, "POST", "/auth/login", `{"username":"external-admin","password":"external-admin-password"}`, "", nil, "")
	expectServingHTTPStatus(t, w, http.StatusOK)
	var info api.SessionInfo
	if err := json.Unmarshal(w.Body.Bytes(), &info); err != nil {
		t.Fatal(err)
	}
	return logoutCookieFrom(t, w, "kudurru_session"), info.CSRF
}
func externalHTTPRow(t *testing.T, f *servingBrowserFixture, admin *http.Cookie) api.ExternalUser {
	t.Helper()
	w := f.call(t, "GET", "/iam/external-users", "", "", admin, "")
	expectServingHTTPStatus(t, w, http.StatusOK)
	var page api.AuthList[api.ExternalUser]
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 {
		t.Fatal("missing external account", err)
	}
	return page.Items[0]
}
func externalCommandBody(t *testing.T, row api.ExternalUser) string {
	t.Helper()
	b, err := json.Marshal(api.ExternalUserCommand{UID: row.UID, Revision: row.Revision})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func TestServingHTTPExternalAccountsExactRevocation(t *testing.T) {
	f := newServingBrowserFixture(t)
	admin, csrf := externalAdminLogin(t, f)
	external, _, externalCSRF := f.logoutLogin(t)
	row := externalHTTPRow(t, f, admin)
	if row.Username != "corp:alice" || row.Disabled || row.UID == row.ProviderUID {
		t.Fatal("lost independent identity")
	}
	// Persisted PAT timestamps have whole-second precision and must actually
	// be later than the original nanosecond identity creation time.
	awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > row.CreatedAt.Unix() })
	issued := f.call(t, "POST", "/auth/tokens", `{"description":"external personal token"}`, "", external, externalCSRF)
	expectServingHTTPStatus(t, issued, http.StatusCreated)
	var pat api.CreatedToken
	if err := json.Unmarshal(issued.Body.Bytes(), &pat); err != nil {
		t.Fatal(err)
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, err := v.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: pat.Token}, time.Now())
		return err == nil
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/external-users", "", pat.Token, nil, ""), http.StatusForbidden)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/disable", externalCommandBody(t, row), "", admin, "wrong"), http.StatusForbidden)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/disable", externalCommandBody(t, row), "", external, externalCSRF), http.StatusForbidden)
	adminIssued := f.call(t, "POST", "/auth/tokens", `{}`, "", admin, csrf)
	expectServingHTTPStatus(t, adminIssued, http.StatusCreated)
	var adminPAT api.CreatedToken
	if err := json.Unmarshal(adminIssued.Body.Bytes(), &adminPAT); err != nil {
		t.Fatal(err)
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		_, err := v.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: adminPAT.Token}, time.Now())
		return err == nil
	})
	srv := httptest.NewServer(f.server)
	defer srv.Close()
	var output strings.Builder
	cli := newRoot()
	cli.SetOut(&output)
	cli.SetErr(&output)
	cli.SetArgs([]string{"--server", srv.URL, "--token", adminPAT.Token, "-o", "json", "iam", "external-users", "disable", row.Name, "--uid", row.UID, "--revision", row.Revision})
	if err := cli.ExecuteContext(t.Context()); err != nil {
		t.Fatal("actual-router CLI disable refused")
	}
	var result map[string]string
	if err := json.Unmarshal([]byte(output.String()), &result); err != nil || result["status"] != "committed" {
		t.Fatal("invalid CLI result", err)
	}
	state := f.security(t)
	if !state.ExternalIdentities[0].Disabled || !state.ExternalSessions[0].Revoked || len(state.AccountFloors) != 1 {
		t.Fatal("incomplete atomic revocation")
	}
	var stored internalv1.APIToken
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: pat.ID}, &stored); err == nil {
		t.Fatal("exact PAT retained")
	}
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		rows, _, err := v.ExternalAccounts()
		return err == nil && len(rows) == 1 && rows[0].Identity.Disabled
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", pat.Token, nil, ""), http.StatusUnauthorized)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", external, ""), http.StatusUnauthorized)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", admin, ""), http.StatusOK)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/enable", externalCommandBody(t, row), "", admin, csrf), http.StatusConflict)
	current := externalHTTPRow(t, f, admin)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/enable", externalCommandBody(t, current), "", admin, csrf), http.StatusNoContent)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		rows, _, err := v.ExternalAccounts()
		return err == nil && len(rows) == 1 && !rows[0].Identity.Disabled
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", pat.Token, nil, ""), http.StatusUnauthorized)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", external, ""), http.StatusUnauthorized)
	// Numeric OP iat must actually pass the newly committed nanosecond floor.
	floor := f.security(t).AccountFloors[0].ValidAfter
	awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > floor.Unix() })
	fresh, _, _ := f.logoutLogin(t)
	current = externalHTTPRow(t, f, admin)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/logout", externalCommandBody(t, current), "", admin, csrf), http.StatusNoContent)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool { _, err := v.CookieSession(fresh.Value, time.Now()); return err != nil })
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", fresh, ""), http.StatusUnauthorized)
	if f.security(t).ExternalIdentities[0].Disabled {
		t.Fatal("logout disabled identity")
	}
}
func TestServingHTTPExternalAccountsInputAndWriterRefusals(t *testing.T) {
	f := newServingBrowserFixture(t)
	f.logoutLogin(t)
	admin, csrf := externalAdminLogin(t, f)
	row := externalHTTPRow(t, f, admin)
	before := f.security(t).Epoch
	for _, body := range []string{`{}`, `null`, `{"uid":null}`, `{"uid":"` + row.UID + `","revision":"` + row.Revision + `","uid":"` + row.UID + `"}`, externalCommandBody(t, row) + ` {}`, `{"uid":"` + row.UID + `","revision":"` + row.Revision + `","extra":true}`} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/disable", body, "", admin, csrf), http.StatusBadRequest)
	}
	for _, query := range []string{"?limit=0", "?limit=101", "?limit=2&limit=3", "?unknown=yes", "?continue=invalid", "?limit=%zz", "?a;b"} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/iam/external-users"+query, "", "", admin, ""), http.StatusBadRequest)
	}
	for _, field := range []string{"uid", "revision"} {
		bad := row
		if field == "uid" {
			bad.UID = strings.Repeat("f", 32)
		} else {
			bad.Revision = strings.Repeat("f", 64)
		}
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+row.Name+"/disable", externalCommandBody(t, bad), "", admin, csrf), http.StatusConflict)
	}
	if f.security(t).Epoch != before {
		t.Fatal("refusal mutated account")
	}
	f.stopWriter()
	w := f.call(t, "POST", "/iam/external-users/"+row.Name+"/disable", externalCommandBody(t, row), "", admin, csrf)
	if w.Code < 400 || f.security(t).Epoch != before {
		t.Fatal("stopped writer admitted mutation")
	}
}

func TestExternalUsersCLIReviewedCommand(t *testing.T) {
	// Full command parser and real router using a local admin credential. The
	// empty inventory is legitimate and malformed identifiers must not send I/O.
	f := newServingHTTPFixture(t, false)
	srv := httptest.NewServer(f.server)
	defer srv.Close()
	var out strings.Builder
	root := newRoot()
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"--server", srv.URL, "--token", f.token, "-o", "json", "iam", "external-users", "list"})
	if err := root.ExecuteContext(t.Context()); err != nil {
		t.Fatal("actual-router list", err)
	}
	var page api.AuthList[api.ExternalUser]
	if err := json.Unmarshal([]byte(out.String()), &page); err != nil || len(page.Items) != 0 {
		t.Fatal("invalid CLI inventory", err)
	}
	// Invalid explicit reviews fail locally, without an automatic GET/retry.
	cmd := newExternalUsersCmd(&cliOpts{})
	cmd.SetArgs([]string{"disable", strings.Repeat("a", 64)})
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	if err := cmd.ExecuteContext(t.Context()); err == nil {
		t.Fatal("missing review admitted")
	}
}

func TestServingHTTPExternalAccountsPaginationAndUnlicensedRevocation(t *testing.T) {
	f := newServingBrowserFixture(t)
	admin, csrf := externalAdminLogin(t, f)
	f.logoutLogin(t)
	state := f.security(t)
	second := state.ExternalIdentities[0]
	second.UID = strings.Repeat("f", 32)
	second.Subject = "different-subject-same-profile"
	state.ExternalIdentities = append(state.ExternalIdentities, second)
	state.Epoch++
	rows, err := servingauth.EncodeSecurity(state)
	if err != nil {
		t.Fatal(err)
	}
	err = f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, row := range rows {
			if err := tx.PutSection(ctx, "security", row); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		items, _, err := v.ExternalAccounts()
		return err == nil && len(items) == 2
	})
	read := func(query string) api.AuthList[api.ExternalUser] {
		w := f.call(t, "GET", "/iam/external-users"+query, "", "", admin, "")
		expectServingHTTPStatus(t, w, http.StatusOK)
		var p api.AuthList[api.ExternalUser]
		if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := read("?limit=1")
	if len(first.Items) != 1 || first.Continue == "" {
		t.Fatal("missing bounded continuation")
	}
	next := read("?limit=1&continue=" + first.Continue)
	if len(next.Items) != 1 || next.Continue != "" || next.Items[0].Name == first.Items[0].Name || next.Items[0].Username != first.Items[0].Username {
		t.Fatal("same-name identities conflated")
	}
	// Losing the provider license must not prevent local administrators from
	// revoking an account. Enabling cannot bypass licensed provider admission.
	_ = f.s.opts.License.Set("invalid-replacement", "test")
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		info, err := v.ProviderInfo("corp")
		return err != nil || info.Availability != servingauth.ProviderReady
	})
	target := first.Items[0]
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+target.Name+"/disable", externalCommandBody(t, target), "", admin, csrf), http.StatusNoContent)
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		items, _, err := v.ExternalAccounts()
		if err != nil {
			return false
		}
		for _, i := range items {
			if i.Name == target.Name {
				return i.Identity.Disabled
			}
		}
		return false
	})
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/external-users?limit=1&continue="+first.Continue, "", "", admin, ""), http.StatusConflict)
	w := f.call(t, "GET", "/iam/external-users/"+target.Name, "", "", admin, "")
	expectServingHTTPStatus(t, w, http.StatusOK)
	var current api.ExternalUser
	if err := json.Unmarshal(w.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/external-users/"+target.Name+"/enable", externalCommandBody(t, current), "", admin, csrf), http.StatusNoContent)
	if f.security(t).ExternalIdentities[0].UID == f.security(t).ExternalIdentities[1].UID {
		t.Fatal("independent identities lost")
	}
}
