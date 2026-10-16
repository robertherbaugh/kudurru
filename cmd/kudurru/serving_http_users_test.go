package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func localUsersFresh(t *testing.T, f *servingHTTPFixture) {
	t.Helper()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		image, err := f.s.opts.Store.ReadGeneration(t.Context())
		return err == nil && v.Base().Same(image.Generation()) && f.s.publisher.Status().ProtectedReady
	})
}

func localUsersRead(t *testing.T, st *sqlite.Store, name string) internalv1.LocalUser {
	t.Helper()
	var u internalv1.LocalUser
	if err := st.Get(t.Context(), store.Key{Kind: "LocalUser", Name: name}, &u); err != nil {
		t.Fatal(err)
	}
	return u
}

func localUsersState(t *testing.T, st *sqlite.Store) servingauth.Security {
	t.Helper()
	image, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func localUsersStage(t *testing.T, f *servingHTTPFixture, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	localUsersFresh(t, f)
}

func localUsersCreate(t *testing.T, f *servingHTTPFixture, name string) api.IAMUser {
	t.Helper()
	localUsersFresh(t, f)
	w := f.call(t, "POST", "/iam/users", factorBody(t, api.CreateLocalUserRequest{Name: name, DisplayName: "Managed person", Password: "temporary-password"}), f.token, nil, "")
	expectServingHTTPStatus(t, w, 201)
	var out api.IAMUser
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UID == "" || out.ResourceVersion == "" {
		t.Fatal("missing final account identity/version")
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || len(w.Result().Cookies()) != 0 {
		t.Fatal("unsafe create headers or enrollment")
	}
	localUsersFresh(t, f)
	return out
}

func localUsersResetBody(t *testing.T, u internalv1.LocalUser) string {
	t.Helper()
	return factorBody(t, api.ResetLocalUserPasswordRequest{UID: string(u.UID), ResourceVersion: u.ResourceVersion, NewPassword: "replacement-password"})
}

func localUsersLogin(t *testing.T, f *servingHTTPFixture, name, password, code string) (*http.Cookie, api.SessionInfo) {
	t.Helper()
	// Independent proofs share the production one-second factor limiter.
	// Reset only test admission, never proof validity or replay state.
	f.h.factorMu.Lock()
	f.h.factorLimit.Reset(name)
	f.h.factorMu.Unlock()
	localUsersFresh(t, f)
	w := f.call(t, "POST", "/auth/login", factorBody(t, api.LoginRequest{Username: name, Password: password, TOTPCode: code}), "", nil, "")
	expectServingHTTPStatus(t, w, 200)
	var info api.SessionInfo
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || info.CSRF == "" || len(w.Result().Cookies()) != 1 {
		t.Fatal("missing enrolled local session")
	}
	localUsersFresh(t, f)
	return w.Result().Cookies()[0], info
}

func TestServingHTTPLocalUsersCreateExactProjectionAndOwnChange(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	before := localUsersState(t, f.s.opts.Store)
	out := localUsersCreate(t, f, "managed-person")
	u := localUsersRead(t, f.s.opts.Store, out.Name)
	if string(u.UID) != out.UID || u.ResourceVersion != out.ResourceVersion || out.PasswordChangedAt == nil || u.Status.PasswordChangedAt == nil || !out.PasswordChangedAt.Equal(u.Status.PasswordChangedAt.Time) {
		t.Fatal("create did not project exact final staged row")
	}
	if store.ManagedBy(&u) != store.ManagerAPI || u.Spec.BuiltinAdmin || u.Spec.Disabled || !u.Spec.MustChangePassword || len(u.Spec.Groups) != 0 || u.Spec.TOTPSecret != "" {
		t.Fatal("create broadened authority or adopted another owner")
	}
	ok, err := f.h.opts.Hasher.Verify(t.Context(), u.Spec.PasswordHash, "temporary-password")
	if err != nil || !ok {
		t.Fatal("created password did not verify")
	}
	after := localUsersState(t, f.s.opts.Store)
	if len(after.Sessions) != len(before.Sessions) || len(after.TOTP) != len(before.TOTP) {
		t.Fatal("create enrolled credentials")
	}
	inventory := f.call(t, "GET", "/iam/users", "", f.token, nil, "")
	expectServingHTTPStatus(t, inventory, 200)
	var listed api.AuthList[api.IAMUser]
	if json.Unmarshal(inventory.Body.Bytes(), &listed) != nil {
		t.Fatal("invalid inventory")
	}
	found := false
	for _, item := range listed.Items {
		if item.Name == out.Name {
			found = reflect.DeepEqual(item, out)
		}
	}
	if !found {
		t.Fatal("create and inventory projections differ")
	}
	duplicate := f.call(t, "POST", "/iam/users", factorBody(t, api.CreateLocalUserRequest{Name: u.Name, Password: "different-password"}), f.token, nil, "")
	expectServingHTTPStatus(t, duplicate, 409)
	unchanged := localUsersRead(t, f.s.opts.Store, u.Name)
	if unchanged.Spec.PasswordHash != u.Spec.PasswordHash || unchanged.UID != u.UID {
		t.Fatal("duplicate overwrote existing account")
	}
	cookie, info := localUsersLogin(t, f, u.Name, "temporary-password", "")
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", factorBody(t, api.CreateLocalUserRequest{Name: "forbidden-child", Password: "temporary-password"}), "", cookie, info.CSRF), 403)
	changed := f.call(t, "POST", "/auth/password", factorBody(t, api.PasswordChangeRequest{CurrentPassword: "temporary-password", NewPassword: "chosen-password"}), "", cookie, info.CSRF)
	expectServingHTTPStatus(t, changed, 200)
	u = localUsersRead(t, f.s.opts.Store, u.Name)
	if store.ManagedBy(&u) != store.ManagerAPI || u.Spec.MustChangePassword {
		t.Fatal("own change stranded or adopted API-managed user")
	}
	localUsersFresh(t, f)
	cookie, _ = localUsersLogin(t, f, u.Name, "chosen-password", "")
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 200)
}

func TestServingHTTPLocalUsersStrictBodiesAndCSRF(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	cookie, info := f.login(t)
	localUsersFresh(t, f)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{} {}`, `{"name":"person","password":null}`, `{"Name":"person","password":"temporary-password"}`,
		`{"name":"person","name":"person","password":"temporary-password"}`, `{"name":"person","password":"temporary-password","groups":["admin"]}`,
		`{"name":"UPPER","password":"temporary-password"}`, `{"name":"person ","password":"temporary-password"}`, `{"name":"person","password":"short"}`,
		`{"name":"person","password":"` + strings.Repeat("x", 1025) + `"}`, `{"name":"person","password":"temporary-password","displayName":"\n"}`,
		`{"name":"person","password":"temporary-password","displayName":"` + strings.Repeat("x", 257) + `"}`,
		string([]byte{'{', '"', 0xff, '"', ':', '"', 'x', '"', '}'}),
	} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", body, f.token, nil, ""), 400)
	}
	for _, body := range []string{`{}`, `{"uid":null}`, `{"UID":"x","resourceVersion":"v","newPassword":"replacement-password"}`, `{"uid":"x","uid":"x","resourceVersion":"v","newPassword":"replacement-password"}`, `{"uid":"x","resourceVersion":"","newPassword":"replacement-password"}`, `{"uid":"x","resourceVersion":"v","newPassword":"replacement-password","force":true}`} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/person/password", body, f.token, nil, ""), 400)
	}
	body := factorBody(t, api.CreateLocalUserRequest{Name: "csrf-person", Password: "temporary-password"})
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", body, "", cookie, ""), 403)
	r := httptest.NewRequest("POST", "http://kudurru.test/api/v1/iam/users", strings.NewReader(body))
	r.AddCookie(cookie)
	r.Header.Set(auth.CSRFHeader, info.CSRF)
	r.Header.Set("Origin", "https://attacker.invalid")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, r)
	expectServingHTTPStatus(t, w, 403)
	for _, path := range []string{"/iam/users", "/iam/users/person/password"} {
		oversized := strings.Repeat(" ", 16<<10) + `{}`
		expectServingHTTPStatus(t, f.call(t, "POST", path, oversized, f.token, nil, ""), 413)
		r := httptest.NewRequest("POST", "http://kudurru.test/api/v1"+path, strings.NewReader(oversized))
		r.Header.Set("Authorization", "Bearer "+f.token)
		w := httptest.NewRecorder()
		f.h.Middleware(f.h.Handler()).ServeHTTP(w, r)
		expectServingHTTPStatus(t, w, 400)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("invalid request changed durable state", err)
	}
	// Current token scope remains an intersection with its private-admin owner.
	created := f.call(t, "POST", "/auth/tokens", `{"scopes":["viewer"]}`, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, created, 201)
	var child api.CreatedToken
	if json.Unmarshal(created.Body.Bytes(), &child) != nil {
		t.Fatal("invalid token response")
	}
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", body, child.Token, nil, ""), 403)
}

func TestServingHTTPLocalUsersResetCASOwnershipAndDisabled(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	created := localUsersCreate(t, f, "disabled-person")
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: created.Name}, &u); err != nil {
			return err
		}
		u.Spec.Disabled = true
		u.Spec.Groups = []string{"retained-group"}
		_, err := tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerAPI})
		return err
	})
	u := localUsersRead(t, f.s.opts.Store, created.Name)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []api.ResetLocalUserPasswordRequest{
		{UID: "other-incarnation", ResourceVersion: u.ResourceVersion, NewPassword: "replacement-password"},
		{UID: string(u.UID), ResourceVersion: created.ResourceVersion, NewPassword: "replacement-password"},
	} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", factorBody(t, req), f.token, nil, ""), 409)
	}
	root := localUsersRead(t, f.s.opts.Store, "root-command")
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/root-command/password", localUsersResetBody(t, root), f.token, nil, ""), 403)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/absent/password", localUsersResetBody(t, u), f.token, nil, ""), 404)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("CAS/ownership rejection changed state", err)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, u), f.token, nil, ""), 204)
	reset := localUsersRead(t, f.s.opts.Store, u.Name)
	if reset.UID != u.UID || !reset.Spec.Disabled || !reflect.DeepEqual(reset.Spec.Groups, u.Spec.Groups) || reset.Spec.DisplayName != u.Spec.DisplayName || !reset.Spec.MustChangePassword || store.ManagedBy(&reset) != store.ManagerAPI {
		t.Fatal("reset modified unrelated account fields")
	}
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", factorBody(t, api.LoginRequest{Username: u.Name, Password: "replacement-password"}), "", nil, ""), 401)
	// A real foreign manager is not adopted, even for an ordinary nonbuiltin.
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "file-person"}, Spec: internalv1.LocalUserSpec{PasswordHash: u.Spec.PasswordHash, MustChangePassword: true}}, store.ApplyOptions{Manager: store.ManagerFile("fixture.yaml")})
		return err
	})
	foreign := localUsersRead(t, f.s.opts.Store, "file-person")
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/file-person/password", localUsersResetBody(t, foreign), f.token, nil, ""), 409)
	cookie, info := localUsersLogin(t, f, foreign.Name, "temporary-password", "")
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/password", factorBody(t, api.PasswordChangeRequest{CurrentPassword: "temporary-password", NewPassword: "chosen-password"}), "", cookie, info.CSRF), 409)
	unchanged := localUsersRead(t, f.s.opts.Store, foreign.Name)
	if !reflect.DeepEqual(foreign.Spec, unchanged.Spec) || store.ManagedBy(&unchanged) != store.ManagerFile("fixture.yaml") {
		t.Fatal("own-password path adopted a foreign manager")
	}
	_ = localUsersCreate(t, f, "deleting-person")
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var deleting internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "deleting-person"}, &deleting); err != nil {
			return err
		}
		deleting.Finalizers = []string{"test.kudurru.io/hold"}
		if _, err := tx.Apply(ctx, &deleting, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		return tx.Delete(ctx, store.KeyOf(&deleting), store.DeleteOptions{Manager: store.ManagerAPI})
	})
	deleting := localUsersRead(t, f.s.opts.Store, "deleting-person")
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/deleting-person/password", localUsersResetBody(t, deleting), f.token, nil, ""), 409)
}

func TestServingHTTPLocalUsersConcurrentCreate(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	body := factorBody(t, api.CreateLocalUserRequest{Name: "one-person", Password: "temporary-password"})
	var wg sync.WaitGroup
	statuses := make(chan int, 2)
	for range 2 {
		wg.Go(func() { statuses <- f.call(t, "POST", "/iam/users", body, f.token, nil, "").Code })
	}
	wg.Wait()
	close(statuses)
	success := 0
	for status := range statuses {
		if status == 201 {
			success++
		} else if status != 409 && status != 503 {
			t.Fatalf("unexpected concurrent create status %d", status)
		}
	}
	if success != 1 {
		t.Fatal("concurrent creation did not commit exactly once")
	}
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", body, f.token, nil, ""), 409)
}

// Fixture-only durable factor install uses the real sealed seed and manifest.
func localUsersInstallFactor(t *testing.T, f *servingHTTPFixture, name string) *totp.Seed {
	t.Helper()
	var seed *totp.Seed
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: name}, &u); err != nil {
			return err
		}
		var err error
		seed, err = totp.Generate(totp.Account{Name: name, UID: string(u.UID)}, 1)
		if err != nil {
			return err
		}
		keys, err := totp.NewKeyring(bytesOfOne(32), nil)
		if err != nil {
			return err
		}
		u.Spec.TOTPSecret, err = keys.Seal(seed)
		if err != nil {
			return err
		}
		u.Spec.MustChangePassword = false
		if _, err = tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		state, err := servingHTTPSecurity(ctx, tx)
		if err != nil {
			return err
		}
		a, version, id := seed.Identity()
		state.Epoch++
		state.TOTP = append(state.TOTP, totp.Record{FormatVersion: totp.FormatVersion, Account: a, SeedVersion: version, SeedID: id, Enabled: true})
		rows, err := servingauth.EncodeSecurity(state)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if err := tx.PutSection(ctx, "security", row); err != nil {
				return err
			}
		}
		return nil
	})
	return seed
}

func localUsersAddToken(t *testing.T, f *servingHTTPFixture, owner internalv1.TokenOwner, manager string, ids ...string) (wire, id string) {
	t.Helper()
	wire, id = tokens.Generate()
	if len(ids) == 1 {
		id = ids[0]
		wire = tokens.Prefix + id + wire[len(tokens.Prefix)+tokens.IDLen:]
	}
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id}, Spec: internalv1.APITokenSpec{Owner: owner, SecretHash: tokens.Hash(wire), Scopes: []string{"viewer"}}}, store.ApplyOptions{Manager: manager, CreateOnly: true})
		return err
	})
	return wire, id
}

func TestServingHTTPLocalUsersResetFactorTokensAndRollback(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	out := localUsersCreate(t, f, "factor-person")
	seed := localUsersInstallFactor(t, f, out.Name)
	cookie, _ := localUsersLogin(t, f, out.Name, "temporary-password", servingHTTPFactorCode(t, seed, time.Now().Add(-30*time.Second)))
	u := localUsersRead(t, f.s.opts.Store, out.Name)
	beforeFactor := localUsersState(t, f.s.opts.Store).TOTP
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: u.Name, UID: string(u.UID), ID: "local:" + u.Name, Provider: "local"}
	wire, id := localUsersAddToken(t, f, owner, store.ManagerSystem)
	legacy := owner
	legacy.Provider = ""
	legacyWire, legacyID := localUsersAddToken(t, f, legacy, store.ManagerSystem)
	preserved := []string{}
	for _, change := range []func(*internalv1.TokenOwner){
		func(o *internalv1.TokenOwner) { o.UID = "another-incarnation" }, func(o *internalv1.TokenOwner) { o.Provider = "external" }, func(o *internalv1.TokenOwner) { o.ID = "local:someone-else" },
		func(o *internalv1.TokenOwner) { o.Kind = internalv1.OwnerServiceAccount; o.Namespace = "apps" },
	} {
		other := owner
		change(&other)
		_, otherID := localUsersAddToken(t, f, other, store.ManagerSystem)
		preserved = append(preserved, otherID)
	}
	// A matching credential with another manager must abort password/status/floor
	// changes and every staged deletion. No Force is allowed, even for reset.
	_, blockedID := localUsersAddToken(t, f, owner, store.ManagerAPI, "zzzzzzzzzzzz")
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, u), f.token, nil, ""), 409)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("failed reset partially committed password/status/floor/tokens", err)
	}
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: blockedID}, store.DeleteOptions{Manager: store.ManagerAPI})
	})
	held, err := f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	component, _ := held.Bundle().Component("auth")
	old := component.(*servingauth.View)
	if _, err := old.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.Cookie, Value: cookie.Value}, time.Now()); err != nil {
		t.Fatal("old cookie was not initially valid")
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, u), f.token, nil, ""), 204)
	reset := localUsersRead(t, f.s.opts.Store, u.Name)
	if reset.Spec.TOTPSecret != u.Spec.TOTPSecret || !reflect.DeepEqual(beforeFactor, localUsersState(t, f.s.opts.Store).TOTP) || !reset.Spec.MustChangePassword {
		t.Fatal("password reset altered factor/replay")
	}
	for _, removed := range []string{id, legacyID} {
		var token internalv1.APIToken
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: removed}, &token); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("exact local token survived reset", err)
		}
	}
	for _, retained := range preserved {
		var token internalv1.APIToken
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: retained}, &token); err != nil {
			t.Fatal("unrelated owner token was removed", err)
		}
	}
	localUsersFresh(t, f)
	for _, proof := range []servingauth.Credential{{Kind: servingauth.Cookie, Value: cookie.Value}, {Kind: servingauth.APIToken, Value: wire}, {Kind: servingauth.APIToken, Value: legacyWire}} {
		awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
			_, err := v.Authenticate(t.Context(), proof, time.Now())
			return errors.Is(err, servingauth.ErrCredentials)
		})
	}
	if _, err := old.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.Cookie, Value: cookie.Value}, time.Now()); err != nil {
		t.Fatal("held immutable cut was mutated")
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", factorBody(t, api.LoginRequest{Username: u.Name, Password: "replacement-password"}), "", nil, ""), 401)
	cookie, info := localUsersLogin(t, f, u.Name, "replacement-password", servingHTTPFactorCode(t, seed, time.Now()))
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/tokens", `{}`, "", cookie, info.CSRF), 403)
	localUsersFresh(t, f)
	// Replay is not reset by the administrator operation.
	f.h.factorMu.Lock()
	f.h.factorLimit.Reset(u.Name)
	f.h.factorMu.Unlock()
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", factorBody(t, api.LoginRequest{Username: u.Name, Password: "replacement-password", TOTPCode: servingHTTPFactorCode(t, seed, time.Now())}), "", nil, ""), 401)
}

func TestServingHTTPLocalUsersSelfResetCookieAndToken(t *testing.T) {
	// The existing captured-provider fixture includes a real signed RBAC license.
	// Every request below is local; no synthetic dependency license is injected.
	browser := newServingBrowserFixture(t)
	defer browser.stopWriter()
	f := browser.servingHTTPFixture
	hash, err := f.h.opts.Hasher.Hash(t.Context(), "initial-password")
	if err != nil {
		t.Fatal(err)
	}
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := tx.Apply(ctx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "managed-admin"}, Spec: internalv1.LocalUserSpec{PasswordHash: hash}}, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
		if err != nil {
			return err
		}
		_, err = tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "managed-admin", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "admin", Scope: publicv1.BindingScope{Kind: "cluster"}, Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "managed-admin"}}}}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	cookie, info := localUsersLogin(t, f, "managed-admin", "initial-password", "")
	u := localUsersRead(t, f.s.opts.Store, "managed-admin")
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: u.Name, UID: string(u.UID), ID: "local:" + u.Name, Provider: "local"}
	token, id := localUsersAddToken(t, f, owner, store.ManagerSystem)
	// The token helper deliberately narrows to viewer; replace its scopes as
	// fixture data so the second self reset exercises real current admin grants.
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var tok internalv1.APIToken
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &tok); err != nil {
			return err
		}
		tok.Spec.Scopes = []string{"admin"}
		_, err := tx.Apply(ctx, &tok, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	w := f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, u), "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, 204)
	factorCleared(t, w)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token, nil, ""), 401)
	reset := localUsersRead(t, f.s.opts.Store, u.Name)
	if reset.Spec.BuiltinAdmin || !reset.Spec.MustChangePassword || store.ManagedBy(&reset) != store.ManagerAPI {
		t.Fatal("self reset broadened or adopted administrator")
	}
	// A new current token is provisioned ONLY as fixture data. Its own successful
	// reset deletes itself; the final proof check must retain original evidence.
	token, id = localUsersAddToken(t, f, owner, store.ManagerSystem)
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var account internalv1.LocalUser
		if err := tx.Get(ctx, store.KeyOf(&reset), &account); err != nil {
			return err
		}
		account.Spec.MustChangePassword = false
		if _, err := tx.Apply(ctx, &account, store.ApplyOptions{Manager: store.ManagerAPI}); err != nil {
			return err
		}
		var tok internalv1.APIToken
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &tok); err != nil {
			return err
		}
		tok.Spec.Scopes = []string{"admin"}
		_, err := tx.Apply(ctx, &tok, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	reset = localUsersRead(t, f.s.opts.Store, u.Name)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, reset), token, nil, ""), 204)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token, nil, ""), 401)
}

func TestServingHTTPLocalUsersFinalClockAndOriginalLease(t *testing.T) {
	for _, changed := range []bool{false, true} {
		for _, regression := range []bool{false, true} {
			name := "expiry"
			if regression {
				name = "regression"
			}
			if changed {
				name += "/changed"
			} else {
				name += "/noop"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				defer f.stop()
				h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1)})
				if err != nil {
					t.Fatal(err)
				}
				prepared, err := h.opts.Hasher.Hash(t.Context(), "prepared-password")
				if err != nil {
					t.Fatal("password preparation failed")
				}
				before, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				called := false
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					err := h.localUserCommand(r, "clock-person", func(ctx context.Context, tx *sqlite.CommandTx, _ auth.Principal, observed time.Time) error {
						called = true
						if !observed.Equal(f.now) {
							return errors.New("mutation clock did not match successful verification")
						}
						if changed {
							if _, err := stageServingLocalUserCreate(ctx, tx, api.CreateLocalUserRequest{Name: "clock-person"}, prepared, observed); err != nil {
								return err
							}
						}
						if regression {
							f.offset.Store(-1)
						} else {
							f.offset.Store(int64(f.expires.Sub(f.now)))
						}
						return nil
					})
					if !errors.Is(err, servingauth.ErrCredentials) {
						t.Error("final credential time was accepted", err)
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				if !called {
					t.Fatal("did not reach current authorized command")
				}
				after, err := f.s.opts.Store.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("final failure committed staged state", err)
				}
			})
		}
	}
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	var released *http.Request
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { released = r })).ServeHTTP(httptest.NewRecorder(), f.request())
	if released == nil {
		t.Fatal("missing original lease")
	}
	called := false
	if err := f.h.localUserCommand(released, "unused", func(context.Context, *sqlite.CommandTx, auth.Principal, time.Time) error { called = true; return nil }); err == nil || called {
		t.Fatal("released original lease repaired")
	}
	f.stop()
	body := factorBody(t, api.CreateLocalUserRequest{Name: "no-writer", Password: "temporary-password"})
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users", body, f.token, nil, ""), 503)
}

func TestServingHTTPLocalUsersResetImageRestartPromotion(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	out := localUsersCreate(t, f, "persist-person")
	seed := localUsersInstallFactor(t, f, out.Name)
	u := localUsersRead(t, f.s.opts.Store, out.Name)
	_, id := localUsersAddToken(t, f, internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: u.Name, UID: string(u.UID), ID: "local:" + u.Name, Provider: "local"}, store.ManagerSystem)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/users/"+u.Name+"/password", localUsersResetBody(t, u), f.token, nil, ""), 204)
	expected := localUsersRead(t, f.s.opts.Store, u.Name)
	state := localUsersState(t, f.s.opts.Store)
	a, version, seedID := seed.Identity()
	if len(state.TOTP) != 1 || state.TOTP[0].Account != a || state.TOTP[0].SeedVersion != version || state.TOTP[0].SeedID != seedID || len(state.AccountFloors) != 1 {
		t.Fatal("reset did not preserve exact replay/floor identity")
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replica.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "localuser-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	defer func() { _ = follower.Close() }()
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "localuser-reset-promotion"}); err != nil {
		t.Fatal(err)
	}
	actual := localUsersRead(t, follower, u.Name)
	if !reflect.DeepEqual(expected.Spec, actual.Spec) || expected.UID != actual.UID || !reflect.DeepEqual(expected.Status, actual.Status) || !reflect.DeepEqual(state, localUsersState(t, follower)) {
		t.Fatal("restart/promotion changed reset/factor/floor state")
	}
	var removed internalv1.APIToken
	if err := follower.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &removed); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("replica revived revoked token", err)
	}
	// A post-promotion owner command still obeys manager/CAS and monotonic floors.
	if err := follower.Command(t.Context(), follower.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := security.Apply(ctx, tx, &actual, store.ApplyOptions{Manager: store.ManagerAPI, ExpectedResourceVersion: actual.ResourceVersion}, time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func TestServingHTTPLocalUsersOwnPasswordExactTokenRevocation(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	out := localUsersCreate(t, f, "own-password-person")
	cookie, info := localUsersLogin(t, f, out.Name, "temporary-password", "")
	u := localUsersRead(t, f.s.opts.Store, out.Name)
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerUser, Name: u.Name, UID: string(u.UID), ID: "local:" + u.Name, Provider: "local"}
	wire, id := localUsersAddToken(t, f, owner, store.ManagerSystem, "aaaaaaaaaaaa")
	legacy := owner
	legacy.Provider = ""
	legacyWire, legacyID := localUsersAddToken(t, f, legacy, store.ManagerSystem)
	// Prove both wire formats actually authenticate before claiming revocation.
	for _, raw := range []string{wire, legacyWire} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", raw, nil, ""), 200)
	}
	retained := []string{}
	for _, change := range []func(*internalv1.TokenOwner){func(o *internalv1.TokenOwner) { o.UID = "other-uid" }, func(o *internalv1.TokenOwner) { o.Provider = "upstream" }, func(o *internalv1.TokenOwner) { o.Kind = internalv1.OwnerServiceAccount; o.Namespace = "apps" }} {
		other := owner
		change(&other)
		_, tokenID := localUsersAddToken(t, f, other, store.ManagerSystem)
		retained = append(retained, tokenID)
	}
	_, blocked := localUsersAddToken(t, f, owner, store.ManagerAPI, "zzzzzzzzzzzz")
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	body := factorBody(t, api.PasswordChangeRequest{CurrentPassword: "temporary-password", NewPassword: "chosen-password", RevokeTokens: true})
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/password", body, "", cookie, info.CSRF), 409)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("foreign token manager did not roll back own password/status/floor/deletions", err)
	}
	for _, raw := range []string{wire, legacyWire} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", raw, nil, ""), 200)
	}
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: blocked}, store.DeleteOptions{Manager: store.ManagerAPI})
	})
	changed := f.call(t, "POST", "/auth/password", body, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, changed, 200)
	current := localUsersRead(t, f.s.opts.Store, u.Name)
	if store.ManagedBy(&current) != store.ManagerAPI || current.Spec.MustChangePassword {
		t.Fatal("own password change adopted account or retained restriction")
	}
	localUsersFresh(t, f)
	for _, removed := range []string{id, legacyID} {
		var row internalv1.APIToken
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: removed}, &row); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("own-password kept matching token", err)
		}
	}
	for _, tokenID := range retained {
		var row internalv1.APIToken
		if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: tokenID}, &row); err != nil {
			t.Fatal("own-password removed unrelated owner", err)
		}
	}
	for _, raw := range []string{wire, legacyWire} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", raw, nil, ""), 401)
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
	if len(changed.Result().Cookies()) != 1 {
		t.Fatal("own password did not return new enrollment")
	}
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", changed.Result().Cookies()[0], ""), 200)
}
