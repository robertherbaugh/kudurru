package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
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
	// A wrong current password is the primary's refusal, relayed.
	if answer := f.request(t, browser.client, http.MethodPost, "/auth/password", "", api.PasswordChangeRequest{CurrentPassword: "not-the-password-1", NewPassword: second}, "X-Kudurru-CSRF", browser.csrf); answer.status != http.StatusUnauthorized {
		t.Fatalf("wrong current password through the follower: %d %s", answer.status, answer.raw)
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
	if answer := f.request(t, nil, http.MethodPost, "/auth/tokens", token, api.CreateTokenRequest{Scopes: []string{"no-such-role"}}); answer.status != http.StatusBadRequest {
		t.Fatalf("invalid token request through the follower: %d %s", answer.status, answer.raw)
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
	if answer := f.request(t, nil, http.MethodGet, "/auth/me", minted.Token, nil); answer.status != http.StatusUnauthorized {
		t.Fatalf("the revoked token on the follower: %d %s", answer.status, answer.raw)
	}
}
