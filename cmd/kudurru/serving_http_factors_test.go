package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha1" //nolint:gosec // Fixed RFC6238 test profile.
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// factorBody never formats credential-bearing values into test failures.
func factorBody(t *testing.T, value any) string {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal("could not encode factor request")
	}
	return string(raw)
}

func factorCode(t *testing.T, secret string, now time.Time) string {
	t.Helper()
	raw, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(secret)
	if err != nil || len(raw) != 20 {
		t.Fatal("invalid provisioning profile")
	}
	defer clear(raw)
	var step [8]byte
	binary.BigEndian.PutUint64(step[:], uint64(now.Unix()/30)) // #nosec G115 -- positive test wall time.
	mac := hmac.New(sha1.New, raw)
	_, _ = mac.Write(step[:])
	digest := mac.Sum(nil)
	offset := int(digest[len(digest)-1] & 15)
	return fmt.Sprintf("%06d", (binary.BigEndian.Uint32(digest[offset:offset+4])&0x7fffffff)%1000000)
}

func factorReady(t *testing.T, f *servingHTTPFixture) {
	t.Helper()
	awaitServingRuntime(t, f.s, func() bool { return f.s.publisher.Status().ProtectedReady })
}

func factorState(t *testing.T, f *servingHTTPFixture) (internalv1.LocalUser, servingauth.Security) {
	t.Helper()
	var u internalv1.LocalUser
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
		t.Fatal(err)
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	state, err := servingauth.DecodeSecurity(image.Payload.Security)
	if err != nil {
		t.Fatal(err)
	}
	return u, state
}

func factorWait(t *testing.T, f *servingHTTPFixture, enabled bool) {
	t.Helper()
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		objects, err := v.Objects("LocalUser")
		if err != nil {
			return false
		}
		for _, object := range objects {
			u := object.(*internalv1.LocalUser)
			if u.Name == "root-command" {
				return (u.Spec.TOTPSecret != "") == enabled
			}
		}
		return false
	})
}

func factorResetAdmission(f *servingHTTPFixture) {
	// Avoid wall-clock sleeps between independent proofs. Separate assertions
	// below exercise the production shared limiter without this fixture reset.
	f.h.factorMu.Lock()
	f.h.factorLimit.Reset("root-command")
	f.h.factorMu.Unlock()
}

func factorSetupHTTP(t *testing.T, f *servingHTTPFixture, cookie *http.Cookie, csrf, oldCode string) api.TOTPSetupResponse {
	t.Helper()
	w := f.call(t, "POST", "/auth/totp/setup", factorBody(t, api.TOTPProofRequest{CurrentPassword: "initial-password", TOTPCode: oldCode}), "", cookie, csrf)
	expectServingHTTPStatus(t, w, 200)
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Referrer-Policy") != "no-referrer" || len(w.Result().Cookies()) != 0 {
		t.Fatal("unsafe setup response headers or unexpected enrollment")
	}
	var setup api.TOTPSetupResponse
	if json.Unmarshal(w.Body.Bytes(), &setup) != nil || setup.Challenge == "" || setup.Secret == "" || !setup.ExpiresAt.After(time.Now()) || setup.ExpiresAt.After(time.Now().Add(5*time.Minute)) {
		t.Fatal("missing bounded provisioning response")
	}
	uri, err := url.Parse(setup.OTPAuthURI)
	if err != nil || uri.Scheme != "otpauth" || uri.Host != "totp" || uri.Path != "/kudurru:root-command" || uri.Query().Get("secret") != setup.Secret || uri.Query().Get("issuer") != "kudurru" || uri.Query().Get("algorithm") != "SHA1" || uri.Query().Get("digits") != "6" || uri.Query().Get("period") != "30" {
		t.Fatal("incorrect local provisioning profile")
	}
	return setup
}

func factorLoginHTTP(t *testing.T, f *servingHTTPFixture, secret string, at time.Time) (*http.Cookie, api.SessionInfo) {
	t.Helper()
	factorResetAdmission(f)
	factorReady(t, f)
	body := factorBody(t, api.LoginRequest{Username: "root-command", Password: "initial-password", TOTPCode: factorCode(t, secret, at)})
	w := f.call(t, "POST", "/auth/login", body, "", nil, "")
	expectServingHTTPStatus(t, w, 200)
	var info api.SessionInfo
	if json.Unmarshal(w.Body.Bytes(), &info) != nil || info.CSRF == "" || len(w.Result().Cookies()) != 1 {
		t.Fatal("factor login did not enroll a session")
	}
	return w.Result().Cookies()[0], info
}

func factorCleared(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	cookies := w.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 || cookies[0].Value != "" || w.Body.Len() != 0 {
		t.Fatal("factor mutation did not clear the old cookie without re-enrollment")
	}
}

func TestServingHTTPFactorsSetupConfirmLoginDisable(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	// Keep only the admission clock fixed: real crypto/commands/publication can
	// exceed one second under race. Credential validity still uses actual time.
	admissionAt := time.Now()
	f.h.factorLimit = users.NewLimiter(users.LimitPolicy{Failures: 1, Window: time.Second, Lockout: time.Second}, func() time.Time { return admissionAt })
	cookie, info := f.login(t)
	status := f.call(t, "GET", "/auth/totp", "", "", cookie, "")
	expectServingHTTPStatus(t, status, 200)
	var out api.TOTPStatus
	if json.Unmarshal(status.Body.Bytes(), &out) != nil || out.Enabled || strings.TrimSpace(status.Body.String()) != `{"enabled":false}` {
		t.Fatal("status was not the safe boolean projection")
	}
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	setup := factorSetupHTTP(t, f, cookie, info.CSRF, "")
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("setup activated a candidate before confirmation", err)
	}
	confirmed := f.call(t, "POST", "/auth/totp/confirm", factorBody(t, api.TOTPConfirmRequest{
		CurrentPassword: "initial-password", Challenge: setup.Challenge, Code: factorCode(t, setup.Secret, time.Now().Add(-30*time.Second)),
	}), "", cookie, info.CSRF)
	expectServingHTTPStatus(t, confirmed, 204)
	factorCleared(t, confirmed)
	u, state := factorState(t, f)
	if u.Spec.TOTPSecret == "" || len(state.TOTP) != 1 || !state.TOTP[0].Enabled || !state.TOTP[0].HasConsumed || len(state.AccountFloors) == 0 {
		t.Fatal("confirmation did not atomically persist seed, replay and revocation")
	}
	consumed := state.TOTP[0].LastConsumedStep
	factorWait(t, f, true)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/totp", "", "", cookie, ""), 401)
	factorResetAdmission(f)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, ""), 401)
	cookie, info = factorLoginHTTP(t, f, setup.Secret, time.Now())
	w := f.call(t, "GET", "/auth/totp", "", "", cookie, "")
	expectServingHTTPStatus(t, w, 200)
	if strings.TrimSpace(w.Body.String()) != `{"enabled":true}` {
		t.Fatal("active status projection incorrect")
	}
	// The successful login and factor management share one admission bucket.
	disableBody := factorBody(t, api.TOTPProofRequest{CurrentPassword: "initial-password", TOTPCode: factorCode(t, setup.Secret, time.Now().Add(30*time.Second))})
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/disable", disableBody, "", cookie, info.CSRF), 429)
	factorResetAdmission(f)
	// Missing old-factor proof cannot turn a valid cookie/password into bypass.
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/disable", `{"currentPassword":"initial-password"}`, "", cookie, info.CSRF), 401)
	disabled := f.call(t, "POST", "/auth/totp/disable", disableBody, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, disabled, 204)
	factorCleared(t, disabled)
	u, state = factorState(t, f)
	if u.Spec.TOTPSecret != "" || len(state.TOTP) != 1 || state.TOTP[0].Enabled || !state.TOTP[0].HasConsumed || state.TOTP[0].LastConsumedStep <= consumed {
		t.Fatal("disable lost replay history or retained active seed")
	}
	factorWait(t, f, false)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
	_, _ = f.login(t)
}

func TestServingHTTPFactorsStrictOwnerBodyAndOrigin(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		`{}`, `null`, `[]`, `{} {}`, `{"currentPassword":null}`,
		`{"currentPassword":"initial-password","currentPassword":"initial-password"}`,
		`{"CurrentPassword":"initial-password"}`, `{"currentPassword":"initial-password","name":"someone-else"}`,
		`{"currentPassword":"initial-password","totpCode":"１２３４５６"}`,
		`{"currentPassword":"initial-password","totpCode":"12345x"}`,
		`{"currentPassword":"` + strings.Repeat("x", 1025) + `"}`,
		string([]byte{'{', '"', 0xff, '"', ':', '"', 'x', '"', '}'}),
	} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", body, "", cookie, info.CSRF), 400)
	}
	// The public forwarder enforces the bound before Huma buffers the body;
	// the directly mounted owner handler independently bounds its decoder.
	oversized := strings.Repeat(" ", 16<<10) + `{}`
	for _, path := range []string{"/auth/totp/setup", "/auth/totp/confirm", "/auth/totp/disable", "/iam/users/root-command/totp/recover"} {
		expectServingHTTPStatus(t, f.call(t, "POST", path, oversized, "", cookie, info.CSRF), 413)
	}
	direct := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/totp/setup", strings.NewReader(oversized))
	direct.AddCookie(cookie)
	direct.Header.Set(auth.CSRFHeader, info.CSRF)
	directResult := httptest.NewRecorder()
	f.h.Middleware(f.h.Handler()).ServeHTTP(directResult, direct)
	expectServingHTTPStatus(t, directResult, 400)
	for _, body := range []string{
		`{"currentPassword":"initial-password","code":"123456","challenge":""}`,
		`{"currentPassword":"initial-password","code":123456,"challenge":"candidate"}`,
		`{"currentPassword":"initial-password","code":"123456","challenge":"` + strings.Repeat("x", 8193) + `"}`,
	} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/confirm", body, "", cookie, info.CSRF), 400)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", `{"currentPassword":"initial-password"}`, "", cookie, ""), 403)
	request := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/totp/setup", strings.NewReader(`{"currentPassword":"initial-password"}`))
	request.AddCookie(cookie)
	request.Header.Set(auth.CSRFHeader, info.CSRF)
	request.Header.Set("Origin", "https://attacker.invalid")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	f.server.ServeHTTP(w, request)
	expectServingHTTPStatus(t, w, 403)
	for _, path := range []string{"/auth/totp", "/auth/totp/setup", "/auth/totp/disable"} {
		method := "POST"
		if path == "/auth/totp" {
			method = "GET"
		}
		expectServingHTTPStatus(t, f.call(t, method, path, `{"currentPassword":"initial-password"}`, f.token, nil, ""), 403)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", `{"currentPassword":"wrong"}`, "", cookie, info.CSRF), 401)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("denied requests changed durable factor state", err)
	}
}

// Install an unconsumed active seed as fixture data, without bypassing HTTP
// authentication in any request. The existing cookie predates this fixture.
func factorInstall(t *testing.T, f *servingHTTPFixture) *totp.Seed {
	t.Helper()
	var seed *totp.Seed
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var u internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &u); err != nil {
			return err
		}
		var err error
		seed, err = totp.Generate(totp.Account{Name: u.Name, UID: string(u.UID)}, 1)
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
		if _, err := tx.Apply(ctx, &u, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
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
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	factorWait(t, f, true)
	return seed
}

func TestServingHTTPFactorsRotationAndConcurrentConfirm(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	seed := factorInstall(t, f)
	setup := factorSetupHTTP(t, f, cookie, info.CSRF, servingHTTPFactorCode(t, seed, time.Now().Add(-30*time.Second)))
	factorReady(t, f)
	factorResetAdmission(f)
	// A candidate code never substitutes for proof of the active old factor.
	missing := factorBody(t, api.TOTPConfirmRequest{CurrentPassword: "initial-password", Challenge: setup.Challenge, Code: factorCode(t, setup.Secret, time.Now())})
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/confirm", missing, "", cookie, info.CSRF), 401)
	factorResetAdmission(f)
	body := factorBody(t, api.TOTPConfirmRequest{CurrentPassword: "initial-password", TOTPCode: servingHTTPFactorCode(t, seed, time.Now()), Challenge: setup.Challenge, Code: factorCode(t, setup.Secret, time.Now())})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		wg.Go(func() { results <- f.call(t, "POST", "/auth/totp/confirm", body, "", cookie, info.CSRF) })
	}
	wg.Wait()
	close(results)
	success := 0
	for w := range results {
		if w.Code == 204 {
			success++
			factorCleared(t, w)
		} else if w.Code != 429 && w.Code != 401 && w.Code != 503 {
			t.Fatalf("unexpected concurrent callback status %d", w.Code)
		}
	}
	if success != 1 {
		t.Fatal("concurrent confirmation did not activate exactly once")
	}
	u, state := factorState(t, f)
	if len(state.TOTP) != 1 || state.TOTP[0].SeedVersion != 2 || !state.TOTP[0].HasConsumed {
		t.Fatal("rotation did not retain one monotonic replay row")
	}
	keys, err := totp.NewKeyring(bytesOfOne(32), nil)
	if err != nil {
		t.Fatal(err)
	}
	current, err := keys.Open(totp.Account{Name: u.Name, UID: string(u.UID)}, u.Spec.TOTPSecret)
	if err != nil || current.ProvisioningSecret() != setup.Secret {
		t.Fatal("rotation installed a different candidate")
	}
	factorWait(t, f, true)
	factorResetAdmission(f)
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/confirm", body, "", cookie, info.CSRF), 401)
	_, _ = factorLoginHTTP(t, f, setup.Secret, time.Now().Add(30*time.Second))
}

func TestServingHTTPFactorsRecoveryUIDScopeAndWriter(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	_ = factorInstall(t, f)
	u, _ := factorState(t, f)
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	path := "/iam/users/root-command/totp/recover"
	expectServingHTTPStatus(t, f.call(t, "POST", path, `{"uid":"wrong-incarnation"}`, f.token, nil, ""), 409)
	for _, body := range []string{`{}`, `{"uid":null}`, `{"uid":"x","uid":"x"}`, `{"UID":"x"}`, `{"uid":"x","name":"other"}`, `{"uid":"` + strings.Repeat("x", 513) + `"}`} {
		expectServingHTTPStatus(t, f.call(t, "POST", path, body, f.token, nil, ""), 400)
	}
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("rejected recovery changed state", err)
	}
	// A real narrowed token cannot borrow its owner's private admin authority.
	created := f.call(t, "POST", "/auth/tokens", `{"scopes":["viewer"]}`, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, created, 201)
	var token api.CreatedToken
	if json.Unmarshal(created.Body.Bytes(), &token) != nil || token.Token == "" {
		t.Fatal("scoped token not created")
	}
	factorReady(t, f)
	body := factorBody(t, api.TOTPRecoverRequest{UID: string(u.UID)})
	expectServingHTTPStatus(t, f.call(t, "POST", path, body, token.Token, nil, ""), 403)
	recovered := f.call(t, "POST", path, body, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, recovered, 204)
	factorCleared(t, recovered)
	u, state := factorState(t, f)
	if u.Spec.TOTPSecret != "" || len(state.TOTP) != 1 || state.TOTP[0].Enabled || len(state.AccountFloors) == 0 {
		t.Fatal("recovery did not disable exact target and revoke old sessions")
	}
	factorWait(t, f, false)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
	// Disabled-factor recovery is a no-op, but still requires the original
	// admitted writer and raw proof. A successor is never sampled here.
	factorReady(t, f)
	expectServingHTTPStatus(t, f.call(t, "POST", path, body, f.token, nil, ""), 204)
	f.stop()
	expectServingHTTPStatus(t, f.call(t, "POST", path, body, f.token, nil, ""), 503)
}

func TestServingHTTPFactorsMustChangeAndFailureAdmission(t *testing.T) {
	t.Run("must-change", func(t *testing.T) {
		f := newServingHTTPFixture(t, true)
		cookie, info := f.login(t)
		expectServingHTTPStatus(t, f.call(t, "GET", "/auth/totp", "", "", cookie, ""), 403)
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", `{"currentPassword":"initial-password"}`, "", cookie, info.CSRF), 403)
	})
	t.Run("failed-password", func(t *testing.T) {
		f := newServingHTTPFixture(t, false)
		cookie, info := f.login(t)
		for range 5 {
			expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", `{"currentPassword":"wrong"}`, "", cookie, info.CSRF), 401)
		}
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/setup", `{"currentPassword":"initial-password"}`, "", cookie, info.CSRF), 429)
		expectServingHTTPStatus(t, f.call(t, "POST", "/auth/login", `{"username":"root-command","password":"initial-password"}`, "", nil, ""), 429)
	})
}

func TestServingHTTPFactorsDisabledNoopOriginalLease(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	body := `{"currentPassword":"initial-password"}`
	before, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	w := f.call(t, "POST", "/auth/totp/disable", body, "", cookie, info.CSRF)
	expectServingHTTPStatus(t, w, 204)
	factorCleared(t, w)
	after, err := f.s.opts.Store.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("already-disabled operation changed durable state", err)
	}
	request := httptest.NewRequest("POST", "http://kudurru.test/api/v1/auth/totp/disable", strings.NewReader(body))
	request.AddCookie(cookie)
	request.Header.Set(auth.CSRFHeader, info.CSRF)
	var released *http.Request
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { released = r })).ServeHTTP(httptest.NewRecorder(), request)
	if released == nil {
		t.Fatal("original lease was not captured")
	}
	denied := httptest.NewRecorder()
	f.h.Handler().ServeHTTP(denied, released)
	expectServingHTTPStatus(t, denied, 503)
	factorReady(t, f)
	f.stop()
	expectServingHTTPStatus(t, f.call(t, "POST", "/auth/totp/disable", body, "", cookie, info.CSRF), 503)
}
