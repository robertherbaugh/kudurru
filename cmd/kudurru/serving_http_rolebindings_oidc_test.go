package main

import (
	"encoding/json"
	"net/http"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestServingHTTPRoleBindingsActualTLSFrozenGrantsAndReplica(t *testing.T) {
	f := newServingBrowserFixture(t)
	admin, csrf := externalAdminLogin(t, f)
	desired := roleBindingManifest("oidc-view", "viewer", "corp:alice")
	binding := roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, desired), "", admin, csrf), http.StatusCreated)
	localUsersFresh(t, f.servingHTTPFixture)
	external, _, externalCSRF := f.logoutLogin(t)
	row := externalHTTPRow(t, f, admin)
	// The token must have a genuinely later whole-second creation time than the
	// persisted identity; no fixture may manufacture a later credential time.
	awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > row.CreatedAt.Unix() })
	issued := f.call(t, "POST", "/auth/tokens", `{"scopes":["viewer"]}`, "", external, externalCSRF)
	expectServingHTTPStatus(t, issued, http.StatusCreated)
	var token api.CreatedToken
	if json.Unmarshal(issued.Body.Bytes(), &token) != nil {
		t.Fatal("personal token response")
	}
	localUsersFresh(t, f.servingHTTPFixture)
	for _, proof := range []struct {
		bearer string
		cookie *http.Cookie
	}{{cookie: external}, {bearer: token.Token}} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/settings", "", proof.bearer, proof.cookie, ""), http.StatusOK)
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, desired), proof.bearer, proof.cookie, externalCSRF), http.StatusForbidden)
	}
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/oidc-view", roleBindingBody(t, roleBindingReview(binding)), "", admin, csrf), http.StatusNoContent)
	localUsersFresh(t, f.servingHTTPFixture)
	floors := f.security(t).GrantFloors
	if len(floors) != 1 || floors[0].Grant != "viewer" {
		t.Fatal("actual router delete omitted durable grant floor")
	}
	for _, proof := range []struct {
		bearer string
		cookie *http.Cookie
	}{{cookie: external}, {bearer: token.Token}} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/settings", "", proof.bearer, proof.cookie, ""), http.StatusUnauthorized)
	}
	replacement := roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, desired), "", admin, csrf), http.StatusCreated)
	if replacement.UID == binding.UID {
		t.Fatal("regrant reused binding incarnation")
	}
	localUsersFresh(t, f.servingHTTPFixture)
	for _, proof := range []struct {
		bearer string
		cookie *http.Cookie
	}{{cookie: external}, {bearer: token.Token}} {
		expectServingHTTPStatus(t, f.call(t, "GET", "/settings", "", proof.bearer, proof.cookie, ""), http.StatusUnauthorized)
	}
	// A genuine new OP enrollment after the original durable floor can receive
	// currently granted permissions; neither the cookie nor PAT above is renewed.
	awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > floors[0].ValidAfter.Unix() })
	fresh, _, _ := f.logoutLogin(t)
	expectServingHTTPStatus(t, f.call(t, "GET", "/settings", "", "", fresh, ""), http.StatusOK)
	image := roleBindingImage(t, f.s.opts.Store)
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal("scheme")
	}
	path := filepath.Join(t.TempDir(), "rolebinding-replica.db")
	open := func() *sqlite.Store {
		st, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: f.s.opts.Validator, InternalKinds: privateStoreKinds(), InitialMode: replication.Follower, Identity: "rolebinding-follower", ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
		if err != nil {
			t.Fatal("follower open")
		}
		return st
	}
	follower := open()
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal("follower enroll")
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal("image validation")
	}
	installed, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: image.Term})
	if err != nil {
		t.Fatal("replica install")
	}
	if err := follower.Close(); err != nil {
		t.Fatal("replica close")
	}
	follower = open()
	defer func() { _ = follower.Close() }()
	if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "rolebinding-restart-promotion"}); err != nil {
		t.Fatal("replica promotion")
	}
	var retained publicv1.RoleBinding
	if err := follower.Get(t.Context(), store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "oidc-view"}, &retained); err != nil || retained.UID != replacement.UID {
		t.Fatal("restart/promotion lost reviewed object")
	}
	replicated := localUsersState(t, follower)
	if !reflect.DeepEqual(replicated.GrantFloors, floors) {
		t.Fatal("restart/promotion erased revocation")
	}
	if !reflect.DeepEqual(replicated.ExternalSessions, f.security(t).ExternalSessions) {
		t.Fatal("replica changed original session proofs")
	}
	// This is durable store replication qualification. Cross-driver Kubernetes
	// runtime/public-writer acceptance remains a separately reviewed contract.
}
