package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/providerhost"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func iamAcceptanceHTTP(t *testing.T, s *servingRuntime, insideAuth ...func(http.Handler) http.Handler) authReloadTLSClient {
	t.Helper()
	h, err := newServingHTTP(servingHTTPOptions{Runtime: s, Hasher: users.NewHasher(1), Cookie: session.CookieConfig{Secure: session.SecureNever}})
	if err != nil {
		t.Fatal(err)
	}
	h.opts.ProviderHandler = h.CapturedBrowserHandler()
	authenticate := h.Middleware
	if len(insideAuth) > 0 {
		authenticate = func(next http.Handler) http.Handler { return h.Middleware(insideAuth[0](next)) }
	}
	router, err := api.New(api.Options{Store: s.opts.Store, Validator: s.opts.Validator, Config: s.opts.Config, CanonicalSettings: s, Authenticate: authenticate, AuthHandler: h.Handler(), Authorizer: h.Authorizer(), Roles: h.Roles, Logger: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(router)
	t.Cleanup(server.Close)
	client := server.Client()
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return authReloadTLSClient{client: client, base: server.URL}
}

type iamAcceptanceCancelKey struct{}

// Every real writer route must reach staging with valid authority, then roll
// back its complete image if its caller expires, time regresses, or the request
// is canceled. The observer changes only memory at the existing command seam.
func TestServingHTTPIAMWritersTLSFinalClockAndCancellation(t *testing.T) {
	for _, writer := range []string{"user-patch", "user-delete", "sa-create", "sa-replace", "sa-delete", "sa-token", "idp-create", "idp-replace", "idp-delete"} {
		for _, mode := range []string{"expiry", "rollback", "cancel"} {
			t.Run(writer+"/"+mode, func(t *testing.T) {
				f := newServingHTTPFixtureOn(t, newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false)), false)
				method, path, body := http.MethodPost, "", ""
				switch writer {
				case "user-patch", "user-delete":
					user := localUsersCreate(t, f, "clock-person")
					path, method = "/iam/users/clock-person", http.MethodPatch
					body = factorBody(t, map[string]any{"uid": user.UID, "resourceVersion": user.ResourceVersion, "disabled": true})
					if writer == "user-delete" {
						method, body = http.MethodDelete, roleBindingBody(t, api.ObjectReview{UID: user.UID, ResourceVersion: user.ResourceVersion})
					}
				case "sa-create", "sa-replace", "sa-delete", "sa-token":
					desired := serviceAccountManifest("clock-robot")
					path, body = "/iam/serviceaccounts", roleBindingBody(t, desired)
					if writer != "sa-create" {
						account := serviceAccountResponse(t, f.call(t, "POST", path, body, f.token, nil, ""), http.StatusCreated)
						localUsersFresh(t, f)
						path = "/iam/serviceaccounts/apps/clock-robot"
						switch writer {
						case "sa-replace":
							desired.Spec.Disabled = true
							method, body = http.MethodPut, roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&account), ServiceAccount: desired})
						case "sa-delete":
							method, body = http.MethodDelete, roleBindingBody(t, objectReview(&account))
						case "sa-token":
							path += "/tokens"
							body = roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(account.UID)})
						}
					}
				case "idp-create", "idp-replace", "idp-delete":
					desired := identityProviderManifest("clock-partner")
					path, body = "/iam/identityproviders", roleBindingBody(t, desired)
					if writer != "idp-create" {
						provider := identityProviderResponse(t, f.call(t, "POST", path, body, f.token, nil, ""), http.StatusCreated)
						localUsersFresh(t, f)
						path = "/iam/identityproviders/clock-partner"
						if writer == "idp-delete" {
							method, body = http.MethodDelete, roleBindingBody(t, objectReview(&provider))
						} else {
							desired.Spec.ClientID = "changed-client"
							method, body = http.MethodPut, roleBindingBody(t, api.ReplaceIdentityProviderCommand{ObjectReview: objectReview(&provider), IdentityProvider: desired})
						}
					}
				}
				localUsersFresh(t, f)
				initial := f.now.Add(time.Minute)
				var offset atomic.Int64
				f.clock.Set(func() time.Time { return initial.Add(time.Duration(offset.Load())) })
				c := iamAcceptanceHTTP(t, f.s, func(next http.Handler) http.Handler {
					return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						ctx, cancel := context.WithCancel(r.Context())
						defer cancel()
						next.ServeHTTP(w, r.WithContext(context.WithValue(ctx, iamAcceptanceCancelKey{}, cancel)))
					})
				})
				before := roleBindingImage(t, f.s.opts.Store)
				var staged atomic.Bool
				f.s.commandHooks.Store(&commandHooks{afterStage: func(ctx context.Context) {
					staged.Store(true)
					switch mode {
					case "expiry":
						offset.Store(int64(f.expires.Sub(initial)))
					case "rollback":
						offset.Store(-1)
					case "cancel":
						ctx.Value(iamAcceptanceCancelKey{}).(context.CancelFunc)()
					}
				}})
				t.Cleanup(func() { f.s.commandHooks.Store(nil) })
				answer := c.call(t, method, path, body, f.token, nil, "")
				if !staged.Load() {
					t.Fatal("writer refused before its actual staging callback completed")
				}
				want := http.StatusUnauthorized
				if mode == "cancel" {
					want = http.StatusServiceUnavailable
				}
				expectServingHTTPStatus(t, answer, want)
				roleBindingUnchanged(t, f.s.opts.Store, before)
			})
		}
	}
}

// Both hops use verified TLS: the real router receives the writer and browser
// calls, and the provider serves discovery, JWKS and code exchange. The
// follower serves the exact original credentials before AND after installing
// the revocation, and again after reopening its database. No mutation retries.
func TestServingHTTPIAMAPIProviderTLSRevocationAndServingFollower(t *testing.T) {
	for _, mutation := range []string{"replace", "delete"} {
		t.Run(mutation, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			admin, csrf := externalAdminLogin(t, f)
			localUsersFresh(t, f.servingHTTPFixture)
			c := iamAcceptanceHTTP(t, f.s)
			desired := identityProviderManifest("partner")
			desired.Spec.Issuer = f.op.Issuer("partner")
			desired.Spec.ClientID = f.op.ClientID
			desired.Spec.ClientSecretRef = &publicv1.CredentialReference{File: f.s.opts.Config.Auth.IdentityProviders[0]["clientSecretFile"].(string)}
			provider := identityProviderResponse(t, c.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, desired), "", admin, csrf), http.StatusCreated)
			awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
				info, err := v.ProviderInfo("partner")
				return err == nil && info.Availability == servingauth.ProviderReady
			})
			localUsersFresh(t, f.servingHTTPFixture)
			binding := roleBindingManifest("partner-view", "viewer", "partner:alice")
			roleBindingResponse(t, c.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, binding), "", admin, csrf), http.StatusCreated)
			localUsersFresh(t, f.servingHTTPFixture)
			follower := iamAcceptanceFollower(t, f, "partner")
			fc := iamAcceptanceHTTP(t, follower.s)
			awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > provider.CreationTimestamp.Unix() })
			start := c.call(t, "GET", "/auth/oidc/partner/start", "", "", nil, "")
			expectServingHTTPStatus(t, start, http.StatusFound)
			callback, err := url.Parse(f.op.Complete(t, start.Header().Get("Location")))
			if err != nil {
				t.Fatal("callback URL")
			}
			pending := logoutCookieFrom(t, start, "kudurru_session_oidc")
			answer := c.call(t, "GET", strings.TrimPrefix(callback.RequestURI(), api.BasePath), "", "", pending, "")
			expectServingHTTPStatus(t, answer, http.StatusFound)
			cookie := logoutCookieFrom(t, answer, "kudurru_session")
			localUsersFresh(t, f.servingHTTPFixture)
			me := c.call(t, "GET", "/auth/me", "", "", cookie, "")
			expectServingHTTPStatus(t, me, http.StatusOK)
			var info api.Me
			if json.Unmarshal(me.Body.Bytes(), &info) != nil {
				t.Fatal("session projection")
			}
			row := externalHTTPRow(t, f, admin)
			awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > row.CreatedAt.Unix() })
			issued := c.call(t, "POST", "/auth/tokens", `{"scopes":["viewer"]}`, "", cookie, info.CSRF)
			expectServingHTTPStatus(t, issued, http.StatusCreated)
			var token api.CreatedToken
			if json.Unmarshal(issued.Body.Bytes(), &token) != nil || token.Token == "" {
				t.Fatal("token projection")
			}
			localUsersFresh(t, f.servingHTTPFixture)
			iamAcceptanceInstall(t, f.s, follower)
			check := func(client authReloadTLSClient, want int) {
				t.Helper()
				expectServingHTTPStatus(t, client.call(t, "GET", "/auth/me", "", "", cookie, ""), want)
				expectServingHTTPStatus(t, client.call(t, "GET", "/auth/me", "", token.Token, nil, ""), want)
			}

			check(c, http.StatusOK)
			check(fc, http.StatusOK)
			if mutation == "delete" {
				expectServingHTTPStatus(t, c.call(t, "DELETE", "/iam/identityproviders/partner", roleBindingBody(t, objectReview(&provider)), "", admin, csrf), http.StatusNoContent)
			} else {
				desired.Spec.ClientID = "replacement-client"
				identityProviderResponse(t, c.call(t, "PUT", "/iam/identityproviders/partner", roleBindingBody(t, api.ReplaceIdentityProviderCommand{ObjectReview: objectReview(&provider), IdentityProvider: desired}), "", admin, csrf), http.StatusOK)
			}
			localUsersFresh(t, f.servingHTTPFixture)
			check(c, http.StatusUnauthorized)
			// Capture the real receiver before the source read. Install through the
			// production image validation boundary while its HTTP server is live.
			installCtx := captureServingReplica(t, follower.store)
			image, err := f.s.opts.Store.ReadGeneration(installCtx)
			if err != nil {
				t.Fatal(err)
			}
			validated, err := follower.store.ValidateImage(installCtx, *image)
			if err != nil {
				t.Fatal(err)
			}
			installed, err := follower.store.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: follower.store.Authority().Installed})
			if err != nil || !installed.Same(image.Generation()) {
				t.Fatal("live follower install", err)
			}
			awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool { return v.Base().Same(image.Generation()) })
			check(fc, http.StatusUnauthorized)
			// The old runtime's Close joins its owners before the same retained
			// database is reopened. Its existing Run cleanup still checks shutdown.
			follower.reopen(t.Context(), t)
			runServingFixture(t, follower.s)
			restarted := iamAcceptanceHTTP(t, follower.s)
			awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool { return v.Base().Same(image.Generation()) })
			check(restarted, http.StatusUnauthorized)
			expectServingHTTPStatus(t, restarted.call(t, "GET", "/auth/me", "", "", admin, ""), http.StatusOK)
		})
	}
}

func TestServingHTTPIAMAccountTLSRevocationAndServingFollower(t *testing.T) {
	for _, kind := range []string{"local", "serviceaccount"} {
		for _, change := range []string{"disable", "delete"} {
			t.Run(kind+"/"+change, func(t *testing.T) {
				f := newServingHTTPFixtureOn(t, newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false)), false)
				c := iamAcceptanceHTTP(t, f.s)
				var token, path, body, method string
				var cookie *http.Cookie
				want := http.StatusOK
				if kind == "local" {
					expectServingHTTPStatus(t, c.call(t, "POST", "/iam/users", `{"name":"revoked-person","password":"temporary-password"}`, f.token, nil, ""), http.StatusCreated)
					localUsersFresh(t, f)
					login := c.call(t, "POST", "/auth/login", `{"username":"revoked-person","password":"temporary-password"}`, "", nil, "")
					expectServingHTTPStatus(t, login, http.StatusOK)
					cookie = logoutCookieFrom(t, login, "kudurru_session")
					localUsersFresh(t, f)
					user := localUsersRead(t, f.s.opts.Store, "revoked-person")
					path, method = "/iam/users/revoked-person", http.MethodPatch
					body = factorBody(t, map[string]any{"uid": user.UID, "resourceVersion": user.ResourceVersion, "disabled": true})
					if change == "delete" {
						body = roleBindingBody(t, objectReview(&user))
					}
				} else {
					desired := serviceAccountManifest("revoked-robot")
					account := serviceAccountResponse(t, c.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, desired), f.token, nil, ""), http.StatusCreated)
					localUsersFresh(t, f)
					issued := c.call(t, "POST", "/iam/serviceaccounts/apps/revoked-robot/tokens", roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(account.UID)}), f.token, nil, "")
					expectServingHTTPStatus(t, issued, http.StatusCreated)
					var credential api.CreatedToken
					if json.Unmarshal(issued.Body.Bytes(), &credential) != nil || credential.Token == "" {
						t.Fatal("issued credential missing")
					}
					token = credential.Token
					localUsersFresh(t, f)
					path, method = "/iam/serviceaccounts/apps/revoked-robot", http.MethodPut
					desired.Spec.Disabled = true
					body = roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&account), ServiceAccount: desired})
					if change == "delete" {
						body = roleBindingBody(t, objectReview(&account))
					}
				}
				if change == "delete" {
					method, want = http.MethodDelete, http.StatusNoContent
				}
				follower := newAnchorFollowerFixture(t.Context(), t, f.s)
				runServingFixture(t, follower.s)
				fc := iamAcceptanceHTTP(t, follower.s)
				check := func(client authReloadTLSClient, status int) {
					t.Helper()
					expectServingHTTPStatus(t, client.call(t, "GET", "/auth/me", "", token, cookie, ""), status)
				}
				check(c, http.StatusOK)
				check(fc, http.StatusOK)
				expectServingHTTPStatus(t, c.call(t, method, path, body, f.token, nil, ""), want)
				localUsersFresh(t, f)
				check(c, http.StatusUnauthorized)
				iamAcceptanceInstall(t, f.s, follower)
				check(fc, http.StatusUnauthorized)
				follower.reopen(t.Context(), t)
				runServingFixture(t, follower.s)
				restarted := iamAcceptanceHTTP(t, follower.s)
				check(restarted, http.StatusUnauthorized)
				expectServingHTTPStatus(t, restarted.call(t, "GET", "/auth/me", "", f.token, nil, ""), http.StatusOK)
			})
		}
	}
}

// Issuance has no idempotency key: concurrent successful requests create
// distinct secrets. Every 201 must account for exactly one durable token;
// a refused request must not leave an extra credential behind.
func TestServingHTTPIAMConcurrentTokenIssuance(t *testing.T) {
	f := newServingHTTPFixtureOn(t, newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false)), false)
	sa := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("tokens")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			<-start
			results <- f.call(t, "POST", "/iam/serviceaccounts/apps/tokens/tokens", roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(sa.UID)}), f.token, nil, "")
		})
	}
	close(start)
	wg.Wait()
	close(results)
	var issued []api.CreatedToken
	for w := range results {
		switch w.Code {
		case http.StatusCreated:
			var token api.CreatedToken
			if json.Unmarshal(w.Body.Bytes(), &token) != nil || token.Token == "" {
				t.Fatal("token answer")
			}
			issued = append(issued, token)
		case http.StatusServiceUnavailable:
		default:
			t.Fatalf("unexpected issuance status %d", w.Code)
		}
	}
	if len(issued) == 0 || ownedTokens(t, f.s.opts.Store, string(sa.UID)) != len(issued) {
		t.Fatal("issuance answers and committed credentials differ")
	}
	if len(issued) == 2 && (issued[0].ID == issued[1].ID || issued[0].Token == issued[1].Token) {
		t.Fatal("independent issuance reused a credential")
	}
	localUsersFresh(t, f)
	for _, token := range issued {
		expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", token.Token, nil, ""), http.StatusOK)
		secretNeverStored(t, f.s.opts.Store, token.Token)
	}
}

// The caller remains valid for an hour. Only the newly issued token expires
// after a second, so its own final check must refuse the transaction. The
// registered fault runs after original caller checks and before token checks.
func TestServingHTTPIAMTokenFinalClockAndCancellation(t *testing.T) {
	for _, mode := range []string{"expiry", "rollback", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f := newServingHTTPFixtureOn(t, newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false)), false)
			sa := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("final-token")), f.token, nil, ""), http.StatusCreated)
			localUsersFresh(t, f)
			initial := time.Now().UTC().Add(time.Minute).Truncate(time.Second)
			var offset atomic.Int64
			f.clock.Set(func() time.Time { return initial.Add(time.Duration(offset.Load())) })
			before := roleBindingImage(t, f.s.opts.Store)
			staged := false
			faultRan := false
			f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				r = r.WithContext(ctx)
				err := f.h.iamCommand(r, []rbac.Attributes{servingHTTPCluster("serviceaccounts", "write"), servingHTTPCluster("tokens", "write")}, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, p auth.Principal, _ time.Time) error {
					if err := tx.RequireCommitCheck(func(context.Context) error {
						faultRan = true
						switch mode {
						case "expiry":
							offset.Store(int64(time.Second))
						case "rollback":
							offset.Store(-1)
						case "cancel":
							cancel()
						}
						return nil
					}); err != nil {
						return err
					}
					created, wire, err := f.h.stageServiceAccountToken(ctx, tx, deps, p, store.Key{Kind: "ServiceAccount", Namespace: "apps", Name: sa.Name}, api.CreateServiceAccountTokenRequest{UID: string(sa.UID)}, time.Second, nil)
					if err != nil {
						return err
					}
					staged = created != nil && wire != ""
					return nil
				})
				if err == nil {
					t.Error("token committed after its deadline, clock rollback or cancellation")
				} else if mode == "cancel" && !errors.Is(err, context.Canceled) || mode != "cancel" && !errors.Is(err, servingauth.ErrCredentials) {
					t.Errorf("unexpected final refusal: %v", err)
				}
			})).ServeHTTP(httptest.NewRecorder(), f.request())
			if !staged || !faultRan {
				t.Fatal("did not exercise the real token staging and commit checks")
			}
			roleBindingUnchanged(t, f.s.opts.Store, before)
			if ownedTokens(t, f.s.opts.Store, string(sa.UID)) != 0 {
				t.Fatal("refused issuance retained a token")
			}
		})
	}
}

func TestServingHTTPIAMServiceAccountWriterWithoutTokensWrite(t *testing.T) {
	command := newServingCommandFixtureOn(t, newServingRuntimeFixtureProviders(t, false), func(s *servingRuntime) {
		input, err := loadProviderStatic(t.Context(), providerStaticOptions{Config: s.opts.Config, Keys: s.opts.AuthKeys, Namespace: "default"})
		if err != nil {
			t.Fatal(err)
		}
		// Install the narrow table before the writer starts, so its original
		// admitted dependency cut includes this permission policy.
		input.Auth.Permissions.Roles["sa-writer"] = []string{"serviceaccounts.*"}
		prepared, err := providerhost.PrepareStatic(t.Context(), input)
		if err != nil {
			input.Release()
			t.Fatal(err)
		}
		defer prepared.Release()
		if err := s.host.ReplaceStatic(t.Context(), prepared); err != nil {
			t.Fatal(err)
		}
	})
	f := newServingHTTPFixtureOn(t, command, false)
	id, _ := tokens.Parse(f.token)
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var token internalv1.APIToken
		if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
			return err
		}
		token.Spec.Scopes = []string{"sa-writer"}
		_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	// The captured caller can write accounts, but cannot write tokens.
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		p, e := v.Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: f.token}, time.Now())
		if e != nil {
			return false
		}
		sa, e := v.Authorize(p, servingHTTPCluster("serviceaccounts", "write"), time.Now())
		if e != nil || !sa.Allowed {
			return false
		}
		token, e := v.Authorize(p, servingHTTPCluster("tokens", "write"), time.Now())
		return e == nil && !token.Allowed
	})
	localUsersFresh(t, f)
	// Account administration succeeds with serviceaccounts.write alone.
	// Minting a credential for that account requires tokens.write as well.
	created := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("forbidden")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	before := roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/serviceaccounts/apps/forbidden/tokens", roleBindingBody(t, api.CreateServiceAccountTokenRequest{UID: string(created.UID)}), f.token, nil, ""), http.StatusForbidden)
	roleBindingUnchanged(t, f.s.opts.Store, before)
}

func TestServingHTTPIAMOwnCookieClearedAfterUpdateAndDelete(t *testing.T) {
	for _, method := range []string{http.MethodPatch, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			admin, csrf := externalAdminLogin(t, f)
			localUsersFresh(t, f.servingHTTPFixture)
			c := iamAcceptanceHTTP(t, f.s)
			created := c.call(t, "POST", "/iam/users", `{"name":"self-admin","password":"temporary-password"}`, "", admin, csrf)
			expectServingHTTPStatus(t, created, http.StatusCreated)
			localUsersFresh(t, f.servingHTTPFixture)
			roleBindingResponse(t, c.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, roleBindingManifest("self-admin", "admin", "self-admin")), "", admin, csrf), http.StatusCreated)
			localUsersFresh(t, f.servingHTTPFixture)
			login := c.call(t, "POST", "/auth/login", `{"username":"self-admin","password":"temporary-password"}`, "", nil, "")
			expectServingHTTPStatus(t, login, http.StatusOK)
			cookie := logoutCookieFrom(t, login, "kudurru_session")
			var info api.SessionInfo
			if json.Unmarshal(login.Body.Bytes(), &info) != nil {
				t.Fatal("login projection")
			}
			expectServingHTTPStatus(t, c.call(t, "POST", "/auth/password", `{"currentPassword":"temporary-password","newPassword":"chosen-password-1"}`, "", cookie, info.CSRF), http.StatusOK)
			localUsersFresh(t, f.servingHTTPFixture)
			login = c.call(t, "POST", "/auth/login", `{"username":"self-admin","password":"chosen-password-1"}`, "", nil, "")
			expectServingHTTPStatus(t, login, http.StatusOK)
			cookie = logoutCookieFrom(t, login, "kudurru_session")
			if json.Unmarshal(login.Body.Bytes(), &info) != nil {
				t.Fatal("login projection")
			}
			localUsersFresh(t, f.servingHTTPFixture)
			user := localUsersRead(t, f.s.opts.Store, "self-admin")
			body := roleBindingBody(t, objectReview(&user))
			want := http.StatusNoContent
			if method == http.MethodPatch {
				body = factorBody(t, map[string]any{"uid": string(user.UID), "resourceVersion": user.ResourceVersion, "disabled": true})
				want = http.StatusOK
			}
			answer := c.call(t, method, "/iam/users/self-admin", body, "", cookie, info.CSRF)
			expectServingHTTPStatus(t, answer, want)
			cleared := false
			for _, entry := range answer.Result().Cookies() {
				if entry.Name == cookie.Name && entry.MaxAge < 0 {
					cleared = true
				}
			}
			if !cleared {
				t.Fatal("successful self-revocation did not clear the caller's cookie")
			}
			localUsersFresh(t, f.servingHTTPFixture)
			expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", "", cookie, ""), http.StatusUnauthorized)
			expectServingHTTPStatus(t, c.call(t, "GET", "/auth/me", "", "", admin, ""), http.StatusOK)
		})
	}
}

func iamAcceptanceInstall(t *testing.T, primary *servingRuntime, follower *anchorFollowerFixture) {
	t.Helper()
	ctx := captureServingReplica(t, follower.store)
	image, err := primary.opts.Store.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.store.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = follower.store.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: image.Term, ExpectedInstalled: follower.store.Authority().Installed}); err != nil {
		t.Fatal(err)
	}
	awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool { return v.Base().Same(image.Generation()) })
}

// A cold follower deliberately treats its first provider entitlement as
// historical. Exercise the real primary's next writer episode so the follower
// observes a new admitted entitlement before issuing the credentials under test.
func iamAcceptanceFollower(t *testing.T, f *servingBrowserFixture, provider string) *anchorFollowerFixture {
	t.Helper()
	follower := newAnchorFollowerFixture(t.Context(), t, f.s)
	runServingFixture(t, follower.s)
	awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool {
		info, e := v.ProviderInfo(provider)
		return e == nil && info.Availability == servingauth.ProviderUnavailable
	})
	var before string
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		info, e := v.ProviderInfo(provider)
		before = info.ResourceIdentity
		return e == nil && info.Availability == servingauth.ProviderReady
	})
	f.stopWriter()
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		done <- f.s.opts.Store.Elector("iam-acceptance-reactivation").Run(ctx, func(ctx context.Context) {
			if err := f.s.RunWriter(ctx); err != nil && ctx.Err() == nil {
				t.Error(err)
			}
		}, nil)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("acceptance writer did not join")
		}
	})
	awaitServingHTTPView(t, f.s, func(v *servingauth.View) bool {
		info, e := v.ProviderInfo(provider)
		return e == nil && info.Availability == servingauth.ProviderReady && info.ResourceIdentity != before && f.s.primary.driver.Status().Ready
	})
	iamAcceptanceInstall(t, f.s, follower)
	awaitServingHTTPView(t, follower.s, func(v *servingauth.View) bool {
		info, e := v.ProviderInfo(provider)
		return e == nil && info.Availability == servingauth.ProviderReady
	})
	// NumericDate tokens must be newer than this writer episode's actual floor.
	state := f.security(t)
	for _, floor := range state.ProviderFloors {
		awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > floor.ValidAfter.Unix() })
	}
	return follower
}
