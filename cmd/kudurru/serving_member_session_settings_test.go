package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	clusterreplica "github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// harnessWrite runs one fixture administration command on the primary.
func harnessWrite(t *testing.T, h *rootMemberHarness, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	writer, err := h.f.s.opts.Store.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.f.s.opts.Store.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), fn); err != nil {
		t.Fatal(err)
	}
	h.f.s.publisher.Wake()
}

// harnessCookieSessions enrolls count local sessions through the forwarded
// login command and seals each against the primary's published view, as a
// follower does. It returns the raw cookies and their session IDs.
func harnessCookieSessions(t *testing.T, h *rootMemberHarness, count int) ([]string, []string) {
	t.Helper()
	hash, err := users.NewHasher(1).Hash(t.Context(), "forwarded-logout-password-1")
	if err != nil {
		t.Fatal(err)
	}
	harnessWrite(t, h, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	var cookies, ids []string
	for range count {
		enrollment, prepared := security.NewForwardedEnrollment()
		// #nosec G117 -- disposable fixture password in a test-only member command
		payload, err := json.Marshal(memberLogin{Username: "root-command", Password: "forwarded-logout-password-1", ClientKey: "192.0.2.201", SessionID: prepared.ID, CSRFHash: prepared.CSRFHash,
			Nonce: "nonce-that-is-long-enough-for-the-forwarded-login-check"})
		if err != nil {
			t.Fatal(err)
		}
		var result memberLoginResult
		if !kudurrutestEventually(10*time.Second, func() bool {
			c := clusterreplica.Command{Format: clusterreplica.CommandFormat, Version: 1, ID: uuid.NewString(), ExpectedTerm: h.f.s.opts.Store.Authority().Term,
				Hops: 1, IssuedAt: h.f.now, NotAfter: h.f.now.Add(time.Minute), Operation: memberLocalLogin,
				Subject: clusterreplica.SubjectRef{Kind: auth.KindUser, ID: "local:root-command", Provider: "local", CredentialKind: "session", CredentialID: prepared.ID, IssuedAt: h.f.now},
				Payload: payload}
			status, out := h.send(c)
			return status == http.StatusOK && json.Unmarshal(out.Body, &result) == nil && result.Committed
		}) {
			t.Fatal("fixture session login refused", result)
		}
		var cookie string
		if !kudurrutestEventually(10*time.Second, func() bool {
			lease, err := h.f.s.publisher.CaptureHTTP(t.Context())
			if err != nil {
				return false
			}
			defer lease.Release()
			value, _ := lease.Bundle().Component("auth")
			cookie, _, err = enrollment.Seal(value.(*servingauth.View), h.f.s.opts.Now())
			return err == nil
		}) {
			t.Fatal("fixture session was not published")
		}
		cookies, ids = append(cookies, cookie), append(ids, prepared.ID)
	}
	return cookies, ids
}

// cookieCommand is h.command with a session cookie as the original credential.
func cookieCommand(t *testing.T, h *rootMemberHarness, id, operation, cookie string, payload any) clusterreplica.Command {
	t.Helper()
	raw := servingauth.Credential{Kind: servingauth.Cookie, Value: cookie}
	var principal auth.Principal
	if !kudurrutestEventually(10*time.Second, func() bool {
		lease, err := h.f.s.publisher.CaptureHTTP(t.Context())
		if err != nil {
			return false
		}
		defer lease.Release()
		value, _ := lease.Bundle().Component("auth")
		principal, err = value.(*servingauth.View).Authenticate(t.Context(), raw, h.f.s.opts.Now())
		return err == nil
	}) {
		t.Fatal("fixture cookie does not verify")
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
	c.Subject = clusterreplica.SubjectRef{Kind: principal.Kind, ID: principal.ID, Provider: principal.Provider, CredentialKind: "session", CredentialID: reference, IssuedAt: principal.CredentialIssuedAt}
	return c
}

// harnessSessionRevoked reads the primary's durable security state.
func harnessSessionRevoked(t *testing.T, h *rootMemberHarness, id string) bool {
	t.Helper()
	revoked, found := false, false
	harnessWrite(t, h, func(ctx context.Context, tx *sqlite.CommandTx) error {
		inventory, err := servingHTTPSecurity(ctx, tx)
		for _, s := range inventory.Sessions {
			if s.ID == id {
				found, revoked = true, s.Revoked
			}
		}
		return err
	})
	if !found {
		t.Fatal("fixture session is not durable", id)
	}
	return revoked
}

func decodeLogout(t *testing.T, out clusterreplica.Outcome) memberLogoutResult {
	t.Helper()
	var result memberLogoutResult
	d := json.NewDecoder(bytes.NewReader(out.Body))
	d.DisallowUnknownFields()
	if err := d.Decode(&result); err != nil {
		t.Fatal("undecodable sign-out result", string(out.Body))
	}
	return result
}

// A forwarded sign-out or session revocation runs on the primary over real
// member mTLS: the primary verifies the raw credential in its transaction,
// derives the session to end from the verified cookie, applies the owner check
// of its own route and retains clean refusals.
func TestRootMemberLogoutCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	cookies, ids := harnessCookieSessions(t, h, 2)
	first, second := cookies[0], cookies[1]

	// An API-token caller has no session to end: a committed no-op.
	status, out := h.send(h.command("token-logout", memberLogout, memberLogoutPayload{}))
	if status != http.StatusOK || out.Status != http.StatusOK || decodeLogout(t, out).Revoked {
		t.Fatal("API-token sign-out was not a no-op", status, out.Status, string(out.Body))
	}
	if harnessSessionRevoked(t, h, ids[0]) || harnessSessionRevoked(t, h, ids[1]) {
		t.Fatal("API-token sign-out revoked a session")
	}
	// A session that is not the caller's, or does not exist, is the route's
	// retained 404 and changes nothing.
	status, out = h.send(cookieCommand(t, h, "foreign", memberLogout, first, memberLogoutPayload{Session: "not-a-session-of-this-account"}))
	var relayed api.Error
	if status != http.StatusOK || out.Status != http.StatusNotFound || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key != api.KeyNotFound {
		t.Fatal("foreign session revocation was not the retained 404", status, out.Status, string(out.Body))
	}
	// A forged subject or a payload of another shape aborts the exchange.
	forged := cookieCommand(t, h, "forged", memberLogout, first, memberLogoutPayload{})
	forged.Subject.ID = "local:someone-else"
	unknown := cookieCommand(t, h, "unknown-field", memberLogout, first, map[string]any{"session": "", "all": true})
	withPrecondition := cookieCommand(t, h, "precondition", memberLogout, first, memberLogoutPayload{})
	withPrecondition.Preconditions = []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: "1"}}
	for name, bad := range map[string]clusterreplica.Command{"forged subject": forged, "unknown field": unknown, "precondition": withPrecondition} {
		if status, out := h.send(bad); *h.sendErr == nil && status == http.StatusOK && out.Status < 400 {
			t.Fatal("invalid sign-out command succeeded:", name)
		}
	}
	if harnessSessionRevoked(t, h, ids[0]) {
		t.Fatal("a refused command revoked the session")
	}

	// The first session revokes the second one of the same account; the exact
	// duplicate answers with the retained outcome and no second effect.
	revoke := cookieCommand(t, h, "revoke-second", memberLogout, first, memberLogoutPayload{Session: ids[1]})
	status, out = h.send(revoke)
	if status != http.StatusOK || out.Status != http.StatusOK || !decodeLogout(t, out).Revoked || !harnessSessionRevoked(t, h, ids[1]) || harnessSessionRevoked(t, h, ids[0]) {
		t.Fatal("own-session revocation not committed exactly", status, out.Status, string(out.Body))
	}
	if status, again := h.send(revoke); status != http.StatusOK || again.Status != out.Status || !bytes.Equal(canonicalOutcome(t, again.Body), canonicalOutcome(t, out.Body)) {
		t.Fatal("dedup replay changed the retained revocation outcome", status)
	}

	// Sign-out revokes the session that the verified cookie names.
	signOut := cookieCommand(t, h, "sign-out", memberLogout, first, memberLogoutPayload{})
	status, out = h.send(signOut)
	if status != http.StatusOK || out.Status != http.StatusOK || !decodeLogout(t, out).Revoked || !harnessSessionRevoked(t, h, ids[0]) {
		t.Fatal("forwarded sign-out not committed", status, out.Status, string(out.Body))
	}
	// The revoked credential cannot recover the retained outcome of its own
	// sign-out.
	if status, out := h.send(signOut); *h.sendErr == nil && status == http.StatusOK && out.Status == http.StatusOK {
		t.Fatal("a revoked credential recovered its retained sign-out outcome")
	}
	// A new sign-out with a credential that the primary no longer accepts,
	// as from a follower whose view lags, is a retained 401 with no effect.
	stale := signOut
	stale.ID = "stale-sign-out"
	if status, out := h.send(stale); status != http.StatusOK || out.Status != http.StatusUnauthorized || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key != api.KeyUnauthenticated {
		t.Fatal("a sign-out with a revoked credential was not the retained 401", status, out.Status)
	}
	image, err := h.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil || bytes.Contains(encoded, []byte(first)) || bytes.Contains(encoded, []byte(second)) {
		t.Fatal("a raw cookie entered the durable image", err)
	}
}

// A forwarded cluster settings patch runs the primary's own validation and
// merge inside the member transaction: one write with its attributed audit
// row, retained public refusals, and the follower's view as precondition.
func TestRootMemberSettingsCommandActualHTTP(t *testing.T) {
	h := newRootMemberHarness(t)
	current := func() public.Kudurru {
		t.Helper()
		var settings public.Kudurru
		if err := h.f.s.opts.Store.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &settings); err != nil {
			t.Fatal(err)
		}
		return settings
	}
	command := func(id, resourceVersion string, patch map[string]any, ifMatch string) clusterreplica.Command {
		c := h.command(id, memberSettingsPatch, memberSettings{Patch: patch, IfMatch: ifMatch, Nonce: "nonce-that-is-long-enough-for-the-forwarded-settings-check"})
		c.Preconditions = []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: resourceVersion}}
		return c
	}
	refused := func(name string, c clusterreplica.Command, want int, key string) {
		t.Helper()
		status, out := h.send(c)
		var relayed api.Error
		if status != http.StatusOK || out.Status != want || json.Unmarshal(out.Body, &relayed) != nil || relayed.Body.Key != key {
			t.Fatalf("%s: want retained %d %s, got status=%d outcome=%d body=%s", name, want, key, status, out.Status, out.Body)
		}
	}
	before := current()
	write := command("settings-write", before.ResourceVersion, map[string]any{"blocking": map[string]any{"blockTTL": "37s"}}, "")
	status, out := h.send(write)
	var body api.SettingsBody
	if status != http.StatusOK || out.Status != http.StatusOK || json.Unmarshal(out.Body, &body) != nil || body.Scope != "cluster" ||
		!slices.Equal(body.Changed, []string{"blocking.blockTTL"}) || body.Persisted == nil || !*body.Persisted {
		t.Fatal("forwarded settings patch refused", status, out.Status, string(out.Body))
	}
	after := current()
	if after.ResourceVersion == before.ResourceVersion || !strings.Contains(string(after.Spec.Settings["blocking"].Raw), "37s") {
		t.Fatal("forwarded settings patch not committed", string(after.Spec.Settings["blocking"].Raw))
	}
	audits, err := h.f.s.opts.Store.ReadAudit(t.Context(), sqlite.AuditQuery{Kind: "Kudurru", Name: "default"})
	if err != nil {
		t.Fatal(err)
	}
	attributed := 0
	for _, entry := range audits {
		if entry.RequestID == "settings-write" && entry.Actor == "User:root-command" && entry.Authz != "" && entry.RequestPath == clusterreplica.PathWrite {
			attributed++
		}
	}
	if attributed != 1 {
		t.Fatal("forwarded settings patch lacks exactly one attributed audit row", attributed)
	}
	// The exact duplicate returns its retained outcome without a second write.
	if status, again := h.send(write); status != http.StatusOK || again.Status != out.Status || !bytes.Equal(canonicalOutcome(t, again.Body), canonicalOutcome(t, out.Body)) || current().ResourceVersion != after.ResourceVersion {
		t.Fatal("dedup replay changed the retained settings outcome", status)
	}

	// The follower's view is the precondition: a stale one is refused.
	refused("stale view", command("stale", before.ResourceVersion, map[string]any{"blocking": map[string]any{"blockTTL": "38s"}}, ""), http.StatusConflict, api.KeyConflict)
	// The route's own refusals, each retained with no effect.
	refused("unknown key", command("unknown", after.ResourceVersion, map[string]any{"noSuchSection": map[string]any{"key": 1}}, ""), http.StatusBadRequest, api.KeyInvalid)
	refused("invalid value", command("invalid", after.ResourceVersion, map[string]any{"blocking": map[string]any{"blockTTL": "not-a-duration"}}, ""), http.StatusBadRequest, api.KeyInvalid)
	refused("empty patch", command("empty", after.ResourceVersion, map[string]any{}, ""), http.StatusBadRequest, api.KeyInvalid)
	refused("node key", command("node-key", after.ResourceVersion, map[string]any{"database": map[string]any{"dsn": "postgres://forwarded-secret-dsn@db.invalid/k"}}, ""), http.StatusBadRequest, api.KeyHostOnly)
	refused("upstreams revision", command("if-match", after.ResourceVersion, map[string]any{"upstreams": map[string]any{"recursion": map[string]any{"dnssec": false}}}, `"7"`), http.StatusPreconditionFailed, api.KeyPrecondition)
	refused("branding without entitlement", command("branding", after.ResourceVersion, map[string]any{"branding": map[string]any{"name": "Forwarded Brand"}}, ""), http.StatusPaymentRequired, api.KeyLicense)
	if current().ResourceVersion != after.ResourceVersion {
		t.Fatal("a refused settings patch changed the settings")
	}

	// Invalid envelopes abort the exchange.
	forged := command("forged", after.ResourceVersion, map[string]any{"blocking": map[string]any{"blockTTL": "39s"}}, "")
	forged.Subject.ID = "someone-else"
	missing := command("no-precondition", after.ResourceVersion, map[string]any{"blocking": map[string]any{"blockTTL": "39s"}}, "")
	missing.Preconditions = nil
	short := h.command("short-nonce", memberSettingsPatch, memberSettings{Patch: map[string]any{"blocking": map[string]any{"blockTTL": "39s"}}, Nonce: "short"})
	short.Preconditions = []clusterreplica.Precondition{{Kind: "Kudurru", Name: "default", ResourceVersion: after.ResourceVersion}}
	for name, bad := range map[string]clusterreplica.Command{"forged subject": forged, "missing precondition": missing, "short nonce": short} {
		if status, out := h.send(bad); *h.sendErr == nil && status == http.StatusOK && out.Status < 400 {
			t.Fatal("invalid settings command succeeded:", name)
		}
	}
	if current().ResourceVersion != after.ResourceVersion {
		t.Fatal("an invalid settings command changed the settings")
	}
	image, err := h.f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(image)
	if err != nil || bytes.Contains(encoded, []byte("forwarded-secret-dsn")) || bytes.Contains(encoded, []byte(h.f.token)) {
		t.Fatal("a refused host-only value or the raw proof entered the durable image", err)
	}
	// A revoked original credential cannot reacquire even its retained result.
	harnessWrite(t, h, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.Delete(ctx, store.Key{Kind: "APIToken", Name: h.ref}, store.DeleteOptions{})
	})
	if status, out := h.send(write); status == http.StatusOK && out.Status == http.StatusOK {
		t.Fatal("revoked credential recovered a forwarded settings outcome")
	}
}

// Through the follower's mounted API and real lease: PATCH /settings, a
// session revocation and a sign-out each send one command to the primary and
// answer with its result; the follower refuses the ended session once it has
// installed the primary's state.
func TestRootMemberForwardSettingsAndSignOutActualHTTP(t *testing.T) {
	f := newMemberForwardHTTPFixture(t, false, memberForwardScheduledPull)
	primaryStore, followerStore := f.primary.s.opts.Store, f.follower.opts.Store
	hash, err := users.NewHasher(1).Hash(t.Context(), "follower-signout-password-1")
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
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.primary.s.publisher.Wake()
	token := f.primary.token

	// Settings: the primary's write, the follower's answer, then convergence.
	var body api.SettingsBody
	answer := f.request(t, nil, http.MethodPatch, "/settings", token, map[string]any{"blocking": map[string]any{"blockTTL": "41s"}})
	if !kudurrutestEventually(20*time.Second, func() bool {
		if answer.status == http.StatusConflict {
			// The fixture's own password write moved the singleton's
			// version only if the follower had not installed it yet.
			answer = f.request(t, nil, http.MethodPatch, "/settings", token, map[string]any{"blocking": map[string]any{"blockTTL": "41s"}})
		}
		return answer.status != http.StatusConflict
	}) || answer.status != http.StatusOK || json.Unmarshal(answer.raw, &body) != nil || body.Node != "receiver" || !slices.Equal(body.Changed, []string{"blocking.blockTTL"}) {
		t.Fatalf("PATCH /settings through the follower: %d %s", answer.status, answer.raw)
	}
	var stored public.Kudurru
	if err := primaryStore.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &stored); err != nil || !strings.Contains(string(stored.Spec.Settings["blocking"].Raw), "41s") {
		t.Fatal("forwarded settings patch not committed on the primary", err)
	}
	if !kudurrutestEventually(20*time.Second, func() bool {
		got := f.request(t, nil, http.MethodGet, "/settings", token, nil)
		return got.status == http.StatusOK && strings.Contains(string(got.raw), `"41s"`)
	}) {
		t.Fatal("follower did not serve the forwarded settings after its pull")
	}
	// The primary's refusal is relayed exactly; nothing is written.
	if answer := f.request(t, nil, http.MethodPatch, "/settings", token, map[string]any{"blocking": map[string]any{"blockTTL": "not-a-duration"}}); answer.status != http.StatusBadRequest || answer.Error.Key != api.KeyInvalid {
		t.Fatalf("invalid settings patch through the follower: %d %s", answer.status, answer.raw)
	}
	// The settings schema no longer marks cluster keys read-only here.
	var schema api.List[api.SettingKey]
	if answer := f.request(t, nil, http.MethodGet, "/settings/_schema", token, nil); answer.status != http.StatusOK || json.Unmarshal(answer.raw, &schema) != nil {
		t.Fatalf("settings schema on the follower: %d %s", answer.status, answer.raw)
	}
	for _, key := range schema.Items {
		if key.Path == "blocking.blockTTL" && key.ReadOnly || key.Path == "log.level" && !key.ReadOnly {
			t.Fatalf("follower settings schema: %s readOnly=%t", key.Path, key.ReadOnly)
		}
	}

	// Two cookie sessions created through the follower's forwarded login.
	login := func() (*http.Client, api.SessionInfo, []*http.Cookie) {
		t.Helper()
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatal(err)
		}
		browser := *f.live.Client()
		browser.Jar = jar
		var info api.SessionInfo
		var answer followerAnswer
		if !kudurrutestEventually(20*time.Second, func() bool {
			answer = f.request(t, &browser, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "root-command", Password: "follower-signout-password-1"})
			return answer.status == http.StatusOK && json.Unmarshal(answer.raw, &info) == nil && info.CSRF != ""
		}) {
			t.Fatalf("login through the follower: %d %s", answer.status, answer.raw)
		}
		target, err := url.Parse(f.live.URL)
		if err != nil {
			t.Fatal(err)
		}
		return &browser, info, jar.Cookies(target)
	}
	// replay presents saved cookies without a jar, as a stolen copy would.
	replay := func(saved []*http.Cookie) int {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, f.live.URL+api.BasePath+"/auth/me", nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, cookie := range saved {
			request.AddCookie(cookie)
		}
		response, err := f.live.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		return response.StatusCode
	}
	revoked := func(id string) bool {
		t.Helper()
		var state bool
		writer, err := primaryStore.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
			inventory, err := servingHTTPSecurity(ctx, tx)
			for _, s := range inventory.Sessions {
				if s.ID == id {
					state = s.Revoked
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return state
	}
	firstClient, first, firstCookies := login()
	secondClient, second, secondCookies := login()
	if replay(firstCookies) != http.StatusOK || replay(secondCookies) != http.StatusOK {
		t.Fatal("follower sessions are not usable")
	}
	var sessions api.List[api.SessionRecordInfo]
	if answer := f.request(t, secondClient, http.MethodGet, "/auth/sessions", "", nil); answer.status != http.StatusOK || json.Unmarshal(answer.raw, &sessions) != nil {
		t.Fatalf("session list on the follower: %d %s", answer.status, answer.raw)
	}
	secondID := ""
	for _, record := range sessions.Items {
		if record.Current {
			secondID = record.ID
		}
	}
	if secondID == "" {
		t.Fatal("the second session is not listed as current")
	}
	_ = second

	// The first session revokes the second through the follower.
	if answer := f.request(t, firstClient, http.MethodDelete, "/auth/sessions/"+secondID, "", nil, "X-Kudurru-CSRF", first.CSRF); answer.status != http.StatusNoContent {
		t.Fatalf("session revocation through the follower: %d %s", answer.status, answer.raw)
	}
	if !revoked(secondID) || !kudurrutestEventually(20*time.Second, func() bool { return replay(secondCookies) == http.StatusUnauthorized }) {
		t.Fatal("revoked session was not ended on the primary or is still accepted by the follower")
	}
	if answer := f.request(t, firstClient, http.MethodDelete, "/auth/sessions/no-such-session", "", nil, "X-Kudurru-CSRF", first.CSRF); answer.status != http.StatusNotFound || answer.Error.Key != api.KeyNotFound {
		t.Fatalf("unknown session revocation through the follower: %d %s", answer.status, answer.raw)
	}
	// An API-token caller signs out without a session to end.
	if answer := f.request(t, nil, http.MethodPost, "/auth/logout", token, nil); answer.status != http.StatusNoContent {
		t.Fatalf("API-token sign-out through the follower: %d %s", answer.status, answer.raw)
	}
	// Sign-out: CSRF first, then the primary revokes the cookie's own session
	// and the follower clears the browser's cookies.
	if answer := f.request(t, firstClient, http.MethodPost, "/auth/logout", "", nil); answer.status != http.StatusForbidden {
		t.Fatalf("sign-out without the CSRF header: %d %s", answer.status, answer.raw)
	}
	before := followerStore.Authority()
	out := f.request(t, firstClient, http.MethodPost, "/auth/logout/browser", "", nil, "X-Kudurru-CSRF", first.CSRF)
	var browserBody api.BrowserLogoutBody
	if out.status != http.StatusOK || json.Unmarshal(out.raw, &browserBody) != nil || browserBody.ContinuePath != "" || len(out.header.Values("Set-Cookie")) == 0 {
		t.Fatalf("browser sign-out through the follower: %d %s", out.status, out.raw)
	}
	firstID := ""
	if err := primaryStore.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		inventory, err := servingHTTPSecurity(ctx, tx)
		for _, s := range inventory.Sessions {
			if s.ID != secondID && s.Revoked {
				firstID = s.ID
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if firstID == "" {
		t.Fatal("browser sign-out did not revoke the cookie's session on the primary")
	}
	if !kudurrutestEventually(20*time.Second, func() bool { return replay(firstCookies) == http.StatusUnauthorized }) {
		t.Fatal("the follower still accepts a signed-out session")
	}
	if followerStore.Authority().InstallSequence == before.InstallSequence {
		t.Fatal("the follower did not install the primary's revocation")
	}
}
