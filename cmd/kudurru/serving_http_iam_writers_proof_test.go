package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// TestServingHTTPIAMWritersConcurrentCreateAndReplace sends the same create
// twice and the same reviewed replacement twice at once. Each commits exactly
// once; the loser gets a conflict, a pending publication or (for a delete) a
// missing object, never a second effect.
func TestServingHTTPIAMWritersConcurrentCreateAndReplace(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	race := func(method, path, body string, want int) {
		t.Helper()
		var wg sync.WaitGroup
		statuses := make(chan int, 2)
		for range 2 {
			wg.Go(func() { statuses <- f.call(t, method, path, body, f.token, nil, "").Code })
		}
		wg.Wait()
		close(statuses)
		success := 0
		for status := range statuses {
			switch status {
			case want:
				success++
			case http.StatusConflict, http.StatusServiceUnavailable:
			case http.StatusNotFound:
				// The losing delete finds the object already gone.
				if method != http.MethodDelete {
					t.Fatalf("%s %s: unexpected concurrent status %d", method, path, status)
				}
			default:
				t.Fatalf("%s %s: unexpected concurrent status %d", method, path, status)
			}
		}
		if success != 1 {
			t.Fatalf("%s %s committed %d times, want exactly once", method, path, success)
		}
		localUsersFresh(t, f)
	}
	race("POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("raced")), http.StatusCreated)
	created := serviceAccountResponse(t, f.call(t, "GET", "/iam/serviceaccounts/apps/raced", "", f.token, nil, ""), http.StatusOK)
	in := serviceAccountManifest("raced")
	in.Spec.Description = "replaced"
	race("PUT", "/iam/serviceaccounts/apps/raced", roleBindingBody(t, api.ReplaceServiceAccountCommand{ObjectReview: objectReview(&created), ServiceAccount: in}), http.StatusOK)
	race("POST", "/iam/identityproviders", roleBindingBody(t, identityProviderManifest("raced")), http.StatusCreated)
	provider := identityProviderResponse(t, f.call(t, "GET", "/iam/identityproviders/raced", "", f.token, nil, ""), http.StatusOK)
	race("DELETE", "/iam/identityproviders/raced", roleBindingBody(t, objectReview(&provider)), http.StatusNoContent)
}

// TestServingHTTPIAMWritersOriginalFinalClockAndCancellation mirrors the D294
// RoleBinding proof for the shared iamCommand: the mutation floor is the
// original verification time, and a credential that expires, a clock that
// regresses or a canceled request before commit leaves the image unchanged.
// A released original lease is never repaired.
func TestServingHTTPIAMWritersOriginalFinalClockAndCancellation(t *testing.T) {
	for _, mode := range []string{"expiry", "rollback", "cancel"} {
		for _, changed := range []bool{false, true} {
			name := mode + "/noop"
			if changed {
				name = mode + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				defer f.stop()
				h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1)})
				if err != nil {
					t.Fatal("handler")
				}
				desired, err := serviceAccountDesired(serviceAccountManifest("final-account"))
				if err != nil {
					t.Fatal("manifest")
				}
				attrs := []rbac.Attributes{iamAttrs("serviceaccounts", "write", "apps", "final-account"), servingHTTPCluster("tokens", "write")}
				before := roleBindingImage(t, f.s.opts.Store)
				called := false
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					r = r.WithContext(ctx)
					err := h.iamCommand(r, attrs, func(ctx context.Context, tx *sqlite.CommandTx, _ servingauth.Dependencies, _ auth.Principal, observed time.Time) error {
						called = true
						if !observed.Equal(f.now) {
							t.Error("floor did not use original verification time")
						}
						if changed {
							if _, err := security.Apply(ctx, tx, desired, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true}, observed); err != nil {
								return err
							}
						}
						switch mode {
						case "expiry":
							f.offset.Store(int64(f.expires.Sub(f.now)))
						case "rollback":
							f.offset.Store(-1)
						case "cancel":
							cancel()
						}
						return nil
					})
					if err == nil {
						t.Error("final refusal ignored")
					}
				})).ServeHTTP(httptest.NewRecorder(), f.request())
				if !called {
					t.Fatal("did not stage exact authorized command")
				}
				roleBindingUnchanged(t, f.s.opts.Store, before)
			})
		}
	}
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	var released *http.Request
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { released = r })).ServeHTTP(httptest.NewRecorder(), f.request())
	if released == nil {
		t.Fatal("original request missing")
	}
	called := false
	err := f.h.iamCommand(released, []rbac.Attributes{iamAttrs("serviceaccounts", "write", "apps", "released")}, func(context.Context, *sqlite.CommandTx, servingauth.Dependencies, auth.Principal, time.Time) error {
		called = true
		return nil
	})
	if err == nil || called {
		t.Fatal("released original proof was repaired")
	}
}

// TestServingHTTPIAMWritersDeleteFloorsSurviveReplicaPromotion installs the
// image after an API delete of a service account (with an issued token) and
// of an IdentityProvider into a follower, restarts and promotes it, and checks
// that the floors, the token deletion and the absence of both objects are
// exactly the leader's, and that the promoted owner still refuses stale reviews.
func TestServingHTTPIAMWritersDeleteFloorsSurviveReplicaPromotion(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	sa := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("replicated")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	issued := issueServiceAccountToken(t, f, sa, http.StatusCreated)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/serviceaccounts/apps/replicated", roleBindingBody(t, objectReview(&sa)), f.token, nil, ""), http.StatusNoContent)
	localUsersFresh(t, f)
	idp := identityProviderResponse(t, f.call(t, "POST", "/iam/identityproviders", roleBindingBody(t, identityProviderManifest("replicated")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/identityproviders/replicated", roleBindingBody(t, objectReview(&idp)), f.token, nil, ""), http.StatusNoContent)
	localUsersFresh(t, f)
	state := localUsersState(t, f.s.opts.Store)
	floors := func(s servingauth.Security) (account, provider bool) {
		for _, a := range s.AccountFloors {
			account = account || a.Account.Kind == publicv1.KindServiceAccount && a.Account.UID == string(sa.UID)
		}
		for _, p := range s.ProviderFloors {
			provider = provider || p.Provider == "replicated" && p.UID == string(idp.UID)
		}
		return account, provider
	}
	if a, p := floors(state); !a || !p {
		t.Fatal("leader lacks the delete floors")
	}

	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replica.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "iam-writers-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	follower := open()
	if err := follower.Enroll(t.Context(), f.s.opts.Store.Authority().Term); err != nil {
		t.Fatal(err)
	}
	installCtx := captureServingReplica(t, follower)
	image, err := f.s.opts.Store.ReadGeneration(installCtx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(installCtx, *image)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := follower.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal(err)
	}
	if err := follower.Close(); err != nil {
		t.Fatal(err)
	}
	follower = open()
	defer func() { _ = follower.Close() }()
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "iam-writers-promotion"}); err != nil {
		t.Fatal(err)
	}
	if a, p := floors(localUsersState(t, follower)); !a || !p {
		t.Fatal("promoted replica lost a delete floor")
	}
	var token internalv1.APIToken
	if err := follower.Get(t.Context(), store.Key{Kind: "APIToken", Name: issued.ID}, &token); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("promoted replica revived the deleted account's token", err)
	}
	var account publicv1.ServiceAccount
	if err := follower.Get(t.Context(), store.Key{Kind: publicv1.KindServiceAccount, Namespace: "apps", Name: "replicated"}, &account); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("promoted replica revived the deleted account", err)
	}
	var provider publicv1.IdentityProvider
	if err := follower.Get(t.Context(), store.Key{Kind: publicv1.KindIdentityProvider, Name: "replicated"}, &provider); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("promoted replica revived the deleted provider", err)
	}
	// A post-promotion owner command still compares the exact reviewed target.
	err = follower.Command(t.Context(), follower.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := reviewedServiceAccount(ctx, tx, store.Key{Kind: publicv1.KindServiceAccount, Namespace: "apps", Name: "replicated"}, objectReview(&sa))
		return err
	})
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatal("promoted owner accepted a review of a deleted account", err)
	}
}

// TestServingHTTPIAMWritersDeleteAtSecurityCapacity documents the bound of
// D396: floors are permanent and share the security section's row cap. When
// the section is full, a delete that needs a new floor is refused as
// unavailable and commits nothing: the account, its token and the image stay
// exactly as they were. Reads keep working.
func TestServingHTTPIAMWritersDeleteAtSecurityCapacity(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	defer f.stop()
	sa := serviceAccountResponse(t, f.call(t, "POST", "/iam/serviceaccounts", roleBindingBody(t, serviceAccountManifest("full")), f.token, nil, ""), http.StatusCreated)
	localUsersFresh(t, f)
	issued := issueServiceAccountToken(t, f, sa, http.StatusCreated)
	localUsersFresh(t, f)
	err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		rows, err := tx.ListSections(ctx, "security")
		if err != nil {
			return err
		}
		for n := range servingauth.MaxRows - len(rows) {
			if err := tx.PutSection(ctx, "security", replication.Entry{Key: fmt.Sprintf("other-owner/%04d", n), Data: []byte(`{}`)}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	f.s.publisher.Wake()
	// A full security section takes long to publish under the race detector
	// (more than the 15 s of localUsersFresh on a loaded machine); wait for
	// the same condition with a longer bound so that the delete below meets a
	// published generation and its 503 is the capacity refusal.
	until := time.Now().Add(90 * time.Second)
	for !servingHTTPFixturePublished(t, f) {
		if time.Now().After(until) {
			t.Fatalf("full security section never published: %+v", f.s.publisher.Status())
		}
		time.Sleep(20 * time.Millisecond)
	}
	before := roleBindingImage(t, f.s.opts.Store)
	w := f.call(t, "DELETE", "/iam/serviceaccounts/apps/full", roleBindingBody(t, objectReview(&sa)), f.token, nil, "")
	expectServingHTTPStatus(t, w, http.StatusServiceUnavailable)
	if !strings.Contains(w.Body.String(), `"key":"unavailable"`) {
		t.Fatalf("delete at capacity answered %s, want the capacity refusal", w.Body.String())
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	if ownedTokens(t, f.s.opts.Store, string(sa.UID)) != 1 {
		t.Fatal("refused delete removed the account's token")
	}
	localUsersFresh(t, f)
	serviceAccountResponse(t, f.call(t, "GET", "/iam/serviceaccounts/apps/full", "", f.token, nil, ""), http.StatusOK)
	expectServingHTTPStatus(t, f.call(t, "GET", "/auth/me", "", issued.Token, nil, ""), http.StatusOK)
}

// servingHTTPFixturePublished is the localUsersFresh condition as one sample.
func servingHTTPFixturePublished(t *testing.T, f *servingHTTPFixture) bool {
	t.Helper()
	l, err := f.s.publisher.CaptureHTTP(t.Context())
	if err != nil {
		return false
	}
	defer l.Release()
	value, ok := l.Bundle().Component("auth")
	v, typed := value.(*servingauth.View)
	if !ok || !typed {
		return false
	}
	image, err := f.s.opts.Store.ReadGeneration(t.Context())
	return err == nil && v.Base().Same(image.Generation()) && f.s.publisher.Status().ProtectedReady
}
