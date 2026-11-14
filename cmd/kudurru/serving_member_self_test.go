package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// tokenCommand is h.command with another API token as the original credential.
func tokenCommand(t *testing.T, h *rootMemberHarness, id, operation, wire string, payload any) clusterreplica.Command {
	t.Helper()
	raw := servingauth.Credential{Kind: servingauth.APIToken, Value: wire}
	var principal auth.Principal
	if !kudurrutestEventually(10*time.Second, func() bool {
		lease, err := h.f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("auth")
		var err2 error
		principal, err2 = value.(*servingauth.View).Authenticate(t.Context(), raw, h.f.s.opts.Now())
		return err2 == nil
	}) {
		t.Fatal("fixture token does not verify")
	}
	proof, err := clusterreplica.NewCredentialProof(raw)
	if err != nil {
		t.Fatal(err)
	}
	reference, err := proof.Reference()
	if err != nil {
		t.Fatal(err)
	}
	c := h.command(id, operation, payload)
	c.Credential = proof
	c.Subject = clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: "api-token", CredentialID: reference, IssuedAt: principal.CredentialIssuedAt}
	return c
}

// tickFixtureClock lets a fixture's fixed clock advance with real time until
// the test ends. A cookie password change issues its new session strictly
// after the account's new floor, as on a real clock.
func tickFixtureClock(t *testing.T, f *servingCommandFixture) {
	t.Helper()
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		last := time.Now()
		for {
			select {
			case <-done:
				return
			case <-time.After(100 * time.Microsecond):
				now := time.Now()
				f.offset.Add(int64(now.Sub(last)))
				last = now
			}
		}
	}()
	t.Cleanup(func() { close(done); <-stopped })
}

func decodeSelf(t *testing.T, out clusterreplica.Outcome) memberSelfResult {
	t.Helper()
	var result memberSelfResult
	d := json.NewDecoder(bytes.NewReader(out.Body))
	d.DisallowUnknownFields()
	if out.Status != http.StatusOK || d.Decode(&result) != nil {
		t.Fatal("undecodable password result", out.Status, string(out.Body))
	}
	return result
}

func harnessLocalUser(t *testing.T, h *rootMemberHarness) internalv1.LocalUser {
	t.Helper()
	var user internalv1.LocalUser
	if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
		t.Fatal(err)
	}
	return user
}

// harnessAuditRows counts the primary's object audit rows for key that
// command id wrote, and those of them that name the original caller with the
// forwarded command's request attribution, as for every other forwarded domain.
func harnessAuditRows(t *testing.T, h *rootMemberHarness, key store.Key, id, method string) (attributed, all int) {
	t.Helper()
	entries, err := h.f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: key.Kind, Name: key.Name, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.RequestID != id {
			continue
		}
		all++
		if entry.Actor == "User:root-command" && entry.Authz != "" && entry.RequestPath == clusterreplica.PathWrite && entry.RequestMethod == method && entry.Result == sqlite.AuditSuccess {
			attributed++
		}
	}
	return attributed, all
}

// forwardedAuditRows returns the primary's audit rows for key that a
// forwarded member command wrote: those attributed to the member write path.
// A refused or replayed command writes none.
func forwardedAuditRows(t *testing.T, primary *sqlite.Store, key store.Key) []sqlite.AuditEntry {
	t.Helper()
	entries, err := primary.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: key.Kind, Name: key.Name, Limit: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var rows []sqlite.AuditEntry
	for _, entry := range entries {
		if entry.RequestPath == clusterreplica.PathWrite {
			rows = append(rows, entry)
		}
	}
	return rows
}

// requireForwardedAudit checks that rows are count rows of one forwarded
// command (one request ID), each attributed to actor with the method, a
// non-empty authorization and a success result, and returns its request ID.
func requireForwardedAudit(t *testing.T, rows []sqlite.AuditEntry, count int, actor, method string) string {
	t.Helper()
	if len(rows) != count {
		t.Fatalf("forwarded command left %d audit rows on the primary, want %d: %+v", len(rows), count, rows)
	}
	id := rows[0].RequestID
	for _, row := range rows {
		if id == "" || row.RequestID != id || row.Actor != actor || row.RequestMethod != method || row.Authz == "" || row.Result != sqlite.AuditSuccess {
			t.Fatalf("forwarded audit row is not attributed to one %s command of %s: %+v", method, actor, row)
		}
	}
	return id
}

func harnessImageContains(t *testing.T, h *rootMemberHarness, secrets ...string) bool {
	t.Helper()
	image, err := h.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if bytes.Contains(encoded, []byte(secret)) {
			return true
		}
	}
	return false
}

// A forwarded password change runs on the primary over real member mTLS with
// the admission of the primary's own route: the caller's credential, the
// current password and factor against the shared counters, and for a cookie
// caller a new session with the follower-prepared ID. Refusals are retained
// with their committed counters; neither password reaches the durable image.
func TestRootMemberPasswordCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	tickFixtureClock(t, h.f)
	const first, second, third = "forwarded-logout-password-1", "forwarded-password-two-2", "forwarded-password-three-3"
	cookies, ids := harnessCookieSessions(t, h, 1)
	hasher := users.NewHasher(1)
	hash := func(password string) string {
		t.Helper()
		out, err := hasher.Hash(t.Context(), password)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	payload := func(current, next string, session security.ForwardedSession) memberPasswordChange {
		return memberPasswordChange{CurrentPassword: current, PasswordHash: hash(next), ClientKey: "192.0.2.210", SessionID: session.ID, CSRFHash: session.CSRFHash,
			Nonce: "nonce-that-is-long-enough-for-the-forwarded-password-check"}
	}
	before := harnessLocalUser(t, h).Spec.PasswordHash

	// A wrong current password is the route's retained 401, with the
	// password counters committed; nothing changes.
	status, out := h.send(h.command("wrong-current", memberPassword, payload("not-the-password-1", second, security.ForwardedSession{})))
	if result := decodeSelf(t, out); status != http.StatusOK || result.Committed || result.Status != http.StatusUnauthorized || !result.Credentials {
		t.Fatal("wrong current password was not the retained 401", status, string(out.Body))
	}
	if harnessLocalUser(t, h).Spec.PasswordHash != before {
		t.Fatal("a refused change changed the password")
	}

	// Commands of another shape abort the exchange without an effect.
	_, prepared := security.NewForwardedEnrollment()
	forged := h.command("forged", memberPassword, payload(first, second, security.ForwardedSession{}))
	forged.Subject.ID = "local:someone-else"
	badHash := payload(first, second, security.ForwardedSession{})
	badHash.PasswordHash = "plaintext-is-no-hash"
	tokenWithSession := h.command("token-session", memberPassword, payload(first, second, prepared))
	cookieWithout := cookieCommand(t, h, "cookie-without-session", memberPassword, cookies[0], payload(first, second, security.ForwardedSession{}))
	unknown := h.command("unknown-field", memberPassword, map[string]any{"currentPassword": first, "passwordHash": hash(second), "clientKey": "x", "nonce": "nonce-that-is-long-enough-for-the-forwarded-password-check", "admin": true})
	for name, bad := range map[string]clusterreplica.Command{"forged subject": forged, "invalid hash": h.command("bad-hash", memberPassword, badHash),
		"token with a session": tokenWithSession, "cookie without a session": cookieWithout, "unknown field": unknown} {
		if status, out := h.send(bad); *h.sendErr == nil && status == http.StatusOK && out.Status < 400 {
			if result := decodeSelf(t, out); result.Committed {
				t.Fatal("invalid password command committed:", name)
			}
		}
	}
	if harnessLocalUser(t, h).Spec.PasswordHash != before {
		t.Fatal("an invalid command changed the password")
	}

	// A cookie caller: the primary changes the password and enrolls the
	// follower-prepared session; the old session ends with the old floor.
	enrollment, session := security.NewForwardedEnrollment()
	change := cookieCommand(t, h, "cookie-change", memberPassword, cookies[0], payload(first, second, session))
	status, out = h.send(change)
	if result := decodeSelf(t, out); status != http.StatusOK || !result.Committed || result.SessionID != session.ID {
		t.Fatal("cookie password change not committed", status, string(out.Body))
	}
	user := harnessLocalUser(t, h)
	if verified, err := hasher.Verify(t.Context(), user.Spec.PasswordHash, second); err != nil || !verified || user.Spec.MustChangePassword {
		t.Fatal("the primary does not store the forwarded hash", err)
	}
	if harnessSessionRevoked(t, h, session.ID) {
		t.Fatal("the new session is not live")
	}
	if !kudurrutestEventually(10*time.Second, func() bool {
		lease, err := h.f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("auth")
		view := value.(*servingauth.View)
		_, oldErr := view.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.Cookie, Value: cookies[0]}, h.f.s.opts.Now())
		_, _, sealErr := enrollment.Seal(view, h.f.s.opts.Now())
		return oldErr != nil && sealErr == nil
	}) {
		t.Fatal("the old cookie still verifies or the prepared session cannot be sealed")
	}
	_ = ids
	// The old cookie cannot recover the retained outcome of its own change,
	// and a new command with it is a retained 401.
	if status, out := h.send(change); *h.sendErr == nil && status == http.StatusOK && decodeSelf(t, out).Committed {
		t.Fatal("a superseded cookie recovered its retained password change")
	}
	stale := change
	stale.ID = "stale-cookie-change"
	if status, out := h.send(stale); status != http.StatusOK || decodeSelf(t, out).Status != http.StatusUnauthorized {
		t.Fatal("a change with a superseded cookie was not the retained 401", status, string(out.Body))
	}

	// A token caller: committed, its exact duplicate answers the retained
	// outcome without a second change.
	tokenChange := h.command("token-change", memberPassword, payload(second, third, security.ForwardedSession{}))
	status, out = h.send(tokenChange)
	if result := decodeSelf(t, out); status != http.StatusOK || !result.Committed || result.SessionID != "" {
		t.Fatal("token password change not committed", status, string(out.Body))
	}
	changed := harnessLocalUser(t, h)
	if status, again := h.send(tokenChange); status != http.StatusOK || !bytes.Equal(canonicalOutcome(t, again.Body), canonicalOutcome(t, out.Body)) || harnessLocalUser(t, h).ResourceVersion != changed.ResourceVersion {
		t.Fatal("dedup replay changed the retained password outcome or wrote again", status)
	}
	// Each committed change leaves the two object audit rows of the shared
	// password body (the account's update and its passwordChangedAt status)
	// in the same commit, both attributed to the original caller; the
	// duplicate adds none.
	userKey := store.Key{Kind: "LocalUser", Name: "root-command"}
	for _, id := range []string{"cookie-change", "token-change"} {
		if attributed, all := harnessAuditRows(t, h, userKey, id, http.MethodPost); attributed != 2 || all != 2 {
			t.Fatal("password change audit is not the two rows attributed to the caller", id, attributed, all)
		}
	}
	if _, all := harnessAuditRows(t, h, userKey, "wrong-current", http.MethodPost); all != 0 {
		t.Fatal("a refused password change wrote the account")
	}

	// Repeated wrong passwords reach the shared lockout: the primary's 429
	// with its Retry-After, retained like any other refusal.
	locked := false
	for i := 0; i < 40 && !locked; i++ {
		_, out := h.send(h.command("lockout-"+string(rune('a'+i%26))+string(rune('a'+i/26)), memberPassword, payload("still-not-the-password", "never-applied-password-9", security.ForwardedSession{})))
		result := decodeSelf(t, out)
		locked = result.Status == http.StatusTooManyRequests && result.Key == "rate_limited" && result.RetryAfterMS > 0
	}
	if !locked {
		t.Fatal("wrong current passwords never reached the shared lockout")
	}
	if harnessLocalUser(t, h).Spec.PasswordHash != changed.Spec.PasswordHash {
		t.Fatal("a refused change changed the password")
	}
	if harnessImageContains(t, h, first, second, third, "not-the-password-1", "still-not-the-password") {
		t.Fatal("a plaintext password entered the durable image")
	}
}

// A forwarded token creation runs the primary's own route body with the
// follower-generated ID and secret hash; a revocation keeps the route's owner
// check and 404. The secret never reaches the primary or its durable image.
func TestRootMemberTokenCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	cluster := rbac.Scope{Kind: rbac.ScopeCluster}
	wire, id := tokens.Generate()
	create := h.command("create", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: id, Request: &api.CreateTokenRequest{Description: "forwarded token"}, SecretHash: tokens.Hash(wire)})
	status, out := h.send(create)
	var result memberTokenResult
	if status != http.StatusOK || decodeMemberTokenResult(out, &result) != nil || result.Token == nil || result.Token.ID != id || !slices.Equal(result.Token.Scopes, []string{"admin"}) || result.Token.Description != "forwarded token" {
		t.Fatal("forwarded token creation not committed", status, out.Status, string(out.Body))
	}
	var stored internalv1.APIToken
	if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &stored); err != nil || stored.Spec.SecretHash != tokens.Hash(wire) || stored.Spec.Owner.ID != "local:root-command" {
		t.Fatal("the primary did not store the forwarded token", err)
	}
	if status, again := h.send(create); status != http.StatusOK || !bytes.Equal(canonicalOutcome(t, again.Body), canonicalOutcome(t, out.Body)) {
		t.Fatal("dedup replay changed the retained token outcome", status)
	}
	// The follower-held secret authenticates on the primary once published.
	created := tokenCommand(t, h, "probe", memberTokens, wire, memberTokenCommand{Action: memberTokenDelete, Scope: cluster, ID: "no-such-token"})
	if status, out := h.send(created); status != http.StatusOK || out.Status != http.StatusNotFound {
		t.Fatal("revoking an unknown token was not the retained 404", status, out.Status, string(out.Body))
	}

	// Clean refusals of the route body are retained and change nothing.
	refusals := map[string]struct {
		command clusterreplica.Command
		status  int
	}{
		"scope wider than the caller's": {h.command("wide", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "wideaaaaaaaa", Request: &api.CreateTokenRequest{Scopes: []string{"viewer"}}, SecretHash: tokens.Hash("wide")}), http.StatusForbidden},
		"invalid lifetime":              {h.command("lifetime", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "lifeaaaaaaaa", Request: &api.CreateTokenRequest{ExpiresIn: "-1h"}, SecretHash: tokens.Hash("life")}), http.StatusBadRequest},
		"unknown scope":                 {h.command("unknown-scope", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "scopeaaaaaaa", Request: &api.CreateTokenRequest{Scopes: []string{"no-such-role"}}, SecretHash: tokens.Hash("scope")}), http.StatusBadRequest},
	}
	for name, tc := range refusals {
		status, out := h.send(tc.command)
		var relayed api.Error
		if status != http.StatusOK || out.Status != tc.status || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key == "" {
			t.Fatal("token refusal not retained:", name, status, out.Status, string(out.Body))
		}
	}
	for _, name := range []string{"wideaaaaaaaa", "lifeaaaaaaaa", "scopeaaaaaaa"} {
		if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: name}, &internalv1.APIToken{}); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("a refused creation stored a token", name, err)
		}
	}
	// Commands of another shape abort the exchange.
	forged := h.command("forged", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "forgedaaaaaa", Request: &api.CreateTokenRequest{}, SecretHash: tokens.Hash("forged")})
	forged.Subject.ID = "local:someone-else"
	for name, bad := range map[string]clusterreplica.Command{
		"forged subject": forged,
		"invalid hash":   h.command("bad-hash", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "hashaaaaaaaa", Request: &api.CreateTokenRequest{}, SecretHash: "not-a-hash"}),
		"invalid ID":     h.command("bad-id", memberTokens, memberTokenCommand{Action: memberTokenCreate, Scope: cluster, ID: "UPPER/case", Request: &api.CreateTokenRequest{}, SecretHash: tokens.Hash("id")}),
		"unknown action": h.command("bad-action", memberTokens, memberTokenCommand{Action: "rotate", Scope: cluster, ID: id}),
		"delete body":    h.command("delete-body", memberTokens, memberTokenCommand{Action: memberTokenDelete, Scope: cluster, ID: id, SecretHash: tokens.Hash("x")}),
	} {
		if status, out := h.send(bad); *h.sendErr == nil && status == http.StatusOK && out.Status < 400 {
			t.Fatal("invalid token command succeeded:", name)
		}
	}

	// The new token revokes itself: committed although the command ends its
	// own credential, which then cannot recover that outcome and is a
	// retained 401 for a new command.
	revoke := tokenCommand(t, h, "self-revoke", memberTokens, wire, memberTokenCommand{Action: memberTokenDelete, Scope: cluster, ID: id})
	status, out = h.send(revoke)
	result = memberTokenResult{}
	if status != http.StatusOK || decodeMemberTokenResult(out, &result) != nil || !result.Deleted {
		t.Fatal("forwarded token revocation not committed", status, out.Status, string(out.Body))
	}
	if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &internalv1.APIToken{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the revoked token is still stored", err)
	}
	// The creation and the revocation each leave one object audit row in
	// their commit, attributed to the original caller.
	tokenKey := store.Key{Kind: "APIToken", Name: id}
	if attributed, all := harnessAuditRows(t, h, tokenKey, "create", http.MethodPost); attributed != 1 || all != 1 {
		t.Fatal("token creation audit is not one row attributed to the caller", attributed, all)
	}
	if attributed, all := harnessAuditRows(t, h, tokenKey, "self-revoke", http.MethodDelete); attributed != 1 || all != 1 {
		t.Fatal("token revocation audit is not one row attributed to the caller", attributed, all)
	}
	if status, out := h.send(revoke); *h.sendErr == nil && status == http.StatusOK && out.Status == http.StatusOK {
		t.Fatal("a revoked token recovered its retained revocation")
	}
	stale := revoke
	stale.ID = "stale-revoke"
	if status, out := h.send(stale); status != http.StatusOK || out.Status != http.StatusUnauthorized {
		t.Fatal("a command with a revoked token was not the retained 401", status, out.Status, string(out.Body))
	}
	if harnessImageContains(t, h, wire) {
		t.Fatal("the token secret entered the durable image")
	}
}

// Through the follower's mounted API and real lease: a user who must change
// the password signs in on the follower, changes it there and gets a new
// cookie that works at once; a personal token created through the follower
// works there at once and is revoked through it. Refusals are the primary's.
func TestRootMemberForwardPasswordAndTokensActualHTTP(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardScheduledPull)
	tickFixtureClock(t, f.primary)
	primaryStore := f.primary.s.opts.Store
	const first, second = "follower-must-change-password-1", "follower-changed-password-2"
	hash, err := users.NewHasher(1).Hash(t.Context(), first)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := primaryStore.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash, user.Spec.MustChangePassword = hash, true
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.primary.s.publisher.Wake()

	browser := followerSignIn(t, f, first)
	if answer := f.request(t, browser.client, http.MethodGet, "/zones", "", nil); answer.status != http.StatusForbidden {
		t.Fatalf("a must-change session reached a route: %d %s", answer.status, answer.raw)
	}
	// A wrong current password is the primary's refusal, relayed. It leaves
	// no audit row on the primary.
	userKey := store.Key{Kind: "LocalUser", Name: "root-command"}
	if answer := f.request(t, browser.client, http.MethodPost, "/auth/password", "", api.PasswordChangeRequest{CurrentPassword: "not-the-password-1", NewPassword: second}, "X-Kudurru-CSRF", browser.csrf); answer.status != http.StatusUnauthorized {
		t.Fatalf("wrong current password through the follower: %d %s", answer.status, answer.raw)
	}
	if rows := forwardedAuditRows(t, primaryStore, userKey); len(rows) != 0 {
		t.Fatalf("a refused forwarded password change left audit rows on the primary: %+v", rows)
	}
	answer := f.request(t, browser.client, http.MethodPost, "/auth/password", "", api.PasswordChangeRequest{CurrentPassword: first, NewPassword: second}, "X-Kudurru-CSRF", browser.csrf)
	var info api.SessionInfo
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &info) != nil || info.MustChangePassword || info.CSRF == "" || len(answer.header.Values("Set-Cookie")) == 0 {
		t.Fatalf("password change through the follower: %d %s", answer.status, answer.raw)
	}
	var user internalv1.LocalUser
	if err := primaryStore.Get(t.Context(), store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil || user.Spec.MustChangePassword {
		t.Fatal("the primary did not commit the change", err)
	}
	if verified, err := users.NewHasher(1).Verify(t.Context(), user.Spec.PasswordHash, second); err != nil || !verified {
		t.Fatal("the primary does not store the new password", err)
	}
	// The change and its passwordChangedAt status are audited on the primary
	// for the caller, under the follower's one member command.
	passwordCommand := requireForwardedAudit(t, forwardedAuditRows(t, primaryStore, userKey), 2, "User:root-command", http.MethodPost)
	// The new cookie works on the follower at once; the old one does not.
	if answer := f.request(t, browser.client, http.MethodGet, "/zones", "", nil); answer.status != http.StatusOK {
		t.Fatalf("the new cookie on the follower: %d %s", answer.status, answer.raw)
	}
	if status := followerCookieStatus(t, f, browser.saved); status != http.StatusUnauthorized {
		t.Fatalf("the cookie from before the change still works on the follower: %d", status)
	}

	// A personal token through the follower works there at once.
	token := f.primary.token
	created := f.request(t, nil, http.MethodPost, "/auth/tokens", token, api.CreateTokenRequest{Description: "through the follower", ExpiresIn: "1h"})
	var minted api.CreatedToken
	if created.status != http.StatusCreated || json.Unmarshal(created.raw, &minted) != nil || minted.Token == "" || minted.ID == "" {
		t.Fatalf("token creation through the follower: %d %s", created.status, created.raw)
	}
	if answer := f.request(t, nil, http.MethodGet, "/auth/me", minted.Token, nil); answer.status != http.StatusOK {
		t.Fatalf("the new token on the follower: %d %s", answer.status, answer.raw)
	}
	var stored internalv1.APIToken
	if err := primaryStore.Get(t.Context(), store.Key{Kind: "APIToken", Name: minted.ID}, &stored); err != nil || stored.Spec.SecretHash != tokens.Hash(minted.Token) {
		t.Fatal("the primary did not store the token", err)
	}
	tokenKey := store.Key{Kind: "APIToken", Name: minted.ID}
	createCommand := requireForwardedAudit(t, forwardedAuditRows(t, primaryStore, tokenKey), 1, "User:root-command", http.MethodPost)
	if createCommand == passwordCommand {
		t.Fatal("two forwarded commands share one audit request ID")
	}
	// The follower refuses a request that it can check itself; the primary
	// refuses a scope wider than the caller's token (admin only), and the
	// follower relays that retained 403. Neither stores a token.
	if answer := f.request(t, nil, http.MethodPost, "/auth/tokens", token, api.CreateTokenRequest{Scopes: []string{"no-such-role"}}); answer.status != http.StatusBadRequest {
		t.Fatalf("invalid token request through the follower: %d %s", answer.status, answer.raw)
	}
	if answer := f.request(t, nil, http.MethodPost, "/auth/tokens", token, api.CreateTokenRequest{Description: "wider than the caller", Scopes: []string{"viewer"}}); answer.status != http.StatusForbidden || answer.Error.Key == "" {
		t.Fatalf("a wider token scope through the follower: %d %s", answer.status, answer.raw)
	}
	objects, err := primaryStore.List(t.Context(), "APIToken", store.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range objects.Items {
		if o.(*internalv1.APIToken).Spec.Description == "wider than the caller" {
			t.Fatal("the primary stored a refused token")
		}
	}
	if answer := f.request(t, nil, http.MethodDelete, "/auth/tokens/nosuchtoken0", token, nil); answer.status != http.StatusNotFound {
		t.Fatalf("revoking an unknown token through the follower: %d %s", answer.status, answer.raw)
	}
	if answer := f.request(t, nil, http.MethodDelete, "/auth/tokens/"+minted.ID, minted.Token, nil); answer.status != http.StatusNoContent {
		t.Fatalf("token revocation through the follower: %d %s", answer.status, answer.raw)
	}
	if err := primaryStore.Get(t.Context(), store.Key{Kind: "APIToken", Name: minted.ID}, &stored); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("the primary still stores the revoked token", err)
	}
	// The revocation adds one row of its own command; the refused revocation
	// of an unknown token left none.
	rows := forwardedAuditRows(t, primaryStore, tokenKey)
	var revoked []sqlite.AuditEntry
	for _, row := range rows {
		if row.RequestID != createCommand {
			revoked = append(revoked, row)
		}
	}
	if len(rows) != 2 {
		t.Fatalf("token create and revoke left %d audit rows on the primary, want 2: %+v", len(rows), rows)
	}
	requireForwardedAudit(t, revoked, 1, "User:root-command", http.MethodDelete)
	if rows := forwardedAuditRows(t, primaryStore, store.Key{Kind: "APIToken", Name: "nosuchtoken0"}); len(rows) != 0 {
		t.Fatalf("a refused forwarded revocation left audit rows on the primary: %+v", rows)
	}
	if answer := f.request(t, nil, http.MethodGet, "/auth/me", minted.Token, nil); answer.status != http.StatusUnauthorized {
		t.Fatalf("the revoked token on the follower: %d %s", answer.status, answer.raw)
	}
}

// setFollowerFixturePassword stores password as root-command's password on the
// fixture's primary and waits until the follower's published view carries it.
func setFollowerFixturePassword(t *testing.T, f *memberForwardHTTPFixture, password string) internalv1.LocalUser {
	t.Helper()
	primaryStore := f.primary.s.opts.Store
	hash, err := users.NewHasher(1).Hash(t.Context(), password)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := primaryStore.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	var user internalv1.LocalUser
	if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.primary.s.publisher.Wake()
	if !followerViewCarriesPassword(t, f, string(user.UID), hash, true) {
		t.Fatal("the follower never served the fixture password")
	}
	return user
}

// followerViewCarriesPassword reports whether the follower's published view
// carries hash for the account; pull pulls the primary's state first.
func followerViewCarriesPassword(t *testing.T, f *memberForwardHTTPFixture, accountUID, hash string, pull bool) bool {
	t.Helper()
	return kudurrutestEventually(10*time.Second, func() bool {
		if pull {
			_ = f.puller.Pull(t.Context())
		}
		lease, err := f.follower.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("auth")
		view, ok := value.(*servingauth.View)
		return ok && viewCarriesPassword(view, accountUID, hash)
	})
}

func primaryPasswordIs(t *testing.T, f *memberForwardHTTPFixture, password string) bool {
	t.Helper()
	var user internalv1.LocalUser
	if err := f.primary.s.opts.Store.Get(t.Context(), store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
		t.Fatal(err)
	}
	verified, err := users.NewHasher(1).Verify(t.Context(), user.Spec.PasswordHash, password)
	return err == nil && verified && !user.Spec.MustChangePassword
}

// A token caller changes its own password through the follower's mounted API.
// The scheduled pull runs once an hour here, so only the follower's explicit
// pull after the commit can install the change: the answer is 200 from the
// follower's view. When the follower cannot serve the change in time (its
// explicit pull is skipped), the committed change is still answered 200 for
// the original account, never `503 unavailable`, which says that nothing was
// sent; revokeTokens revokes the calling token, which the follower refuses
// once it installs the primary's state.
func TestRootMemberForwardTokenCallerPasswordActualHTTP(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardRarePull)
	tickFixtureClock(t, f.primary)
	const first, second, third = "token-caller-password-first-1", "token-caller-password-second-2", "token-caller-password-third-3"
	setFollowerFixturePassword(t, f, first)
	token := f.primary.token

	answer := f.request(t, nil, http.MethodPost, "/auth/password", token, api.PasswordChangeRequest{CurrentPassword: first, NewPassword: second})
	var info api.SessionInfo
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &info) != nil || info.MustChangePassword || info.Principal.ID != "local:root-command" || info.Principal.TokenID == "" {
		t.Fatalf("a token caller's password change through the follower: %d %s", answer.status, answer.raw)
	}
	if !primaryPasswordIs(t, f, second) {
		t.Fatal("the primary did not commit the change")
	}
	var mirrored internalv1.LocalUser
	if err := f.follower.opts.Store.Get(t.Context(), store.Key{Kind: "LocalUser", Name: "root-command"}, &mirrored); err != nil {
		t.Fatal(err)
	}
	if verified, err := users.NewHasher(1).Verify(t.Context(), mirrored.Spec.PasswordHash, second); err != nil || !verified {
		t.Fatal("the follower answered before it installed the change", err)
	}

	// The follower cannot serve the next change: the committed change is
	// still answered for the account, and it revokes the calling token.
	f.skipSelfPull.Store(true)
	answer = f.request(t, nil, http.MethodPost, "/auth/password", token, api.PasswordChangeRequest{CurrentPassword: second, NewPassword: third, RevokeTokens: true})
	info = api.SessionInfo{}
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &info) != nil || info.MustChangePassword || info.Principal.ID != "local:root-command" {
		t.Fatalf("a committed password change the follower could not serve yet: %d %s", answer.status, answer.raw)
	}
	if !primaryPasswordIs(t, f, third) {
		t.Fatal("the primary did not commit the second change")
	}
	if remaining, err := f.primary.s.opts.Store.List(t.Context(), "APIToken", store.ListOptions{}); err != nil || len(remaining.Items) != 0 {
		t.Fatal("revokeTokens left a token on the primary", len(remaining.Items), err)
	}
	f.skipSelfPull.Store(false)
	if err := f.puller.Pull(t.Context()); err != nil {
		t.Fatal("the follower could not pull the primary's state", err)
	}
	if !kudurrutestEventually(10*time.Second, func() bool {
		return f.request(t, nil, http.MethodGet, "/auth/me", token, nil).status == http.StatusUnauthorized
	}) {
		t.Fatal("the revoked token still works on the follower")
	}
}

// A cookie caller's change that commits while the follower cannot install the
// new session within the route's bound is answered `503
// committed_output_unavailable`, never `unavailable`: the password changed,
// and the caller signs in with the new one.
func TestRootMemberForwardCookiePasswordUnservedActualHTTP(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardRarePull)
	tickFixtureClock(t, f.primary)
	const first, second = "cookie-unserved-password-first-1", "cookie-unserved-password-second-2"
	setFollowerFixturePassword(t, f, first)
	browser := followerSignIn(t, f, first)
	f.skipSelfPull.Store(true)
	encoded, err := json.Marshal(api.PasswordChangeRequest{CurrentPassword: first, NewPassword: second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.live.URL+api.BasePath+"/auth/password", bytes.NewReader(encoded))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Kudurru-CSRF", browser.csrf)
	response, err := browser.client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	var refused api.Error
	if err := json.NewDecoder(response.Body).Decode(&refused); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusServiceUnavailable || refused.Body.Key != "committed_output_unavailable" ||
		!strings.Contains(refused.Body.Message, "could not install the new session in time") {
		t.Fatalf("an unserved committed cookie change: %d %+v", response.StatusCode, refused.Body)
	}
	if !primaryPasswordIs(t, f, second) {
		t.Fatal("the primary did not commit the change")
	}
}
