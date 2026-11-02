package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func roleBindingManifest(name, role, subject string) api.RoleBindingDesired {
	return api.RoleBindingDesired{APIVersion: publicv1.GroupVersion.String(), Kind: publicv1.KindRoleBinding, Metadata: api.RoleBindingMetadata{Name: name, Namespace: "apps"}, Spec: api.RoleBindingFields{Role: role, Subjects: []api.BindingSubject{{Kind: "User", Name: subject}}}}
}
func roleBindingBody(t *testing.T, in any) string {
	t.Helper()
	raw, err := json.Marshal(in)
	if err != nil {
		t.Fatal("encode command")
	}
	return string(raw)
}
func roleBindingReview(obj publicv1.RoleBinding) api.RoleBindingReview {
	return api.RoleBindingReview{UID: string(obj.UID), ResourceVersion: obj.ResourceVersion}
}
func roleBindingResponse(t *testing.T, w *httptest.ResponseRecorder, status int) publicv1.RoleBinding {
	t.Helper()
	expectServingHTTPStatus(t, w, status)
	var out publicv1.RoleBinding
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.UID == "" || out.ResourceVersion == "" || store.ManagedBy(&out) == "" {
		t.Fatal("missing reviewed public metadata")
	}
	return out
}
func roleBindingUnchanged(t *testing.T, s *sqlite.Store, before *replication.Image) {
	t.Helper()
	after, err := s.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) || !reflect.DeepEqual(before.Payload, after.Payload) {
		t.Fatal("refusal changed object/security/audit/generation")
	}
}
func roleBindingImage(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	out, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal("read generation")
	}
	return out
}

func TestServingHTTPRoleBindingsReviewedCRUD(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	in := roleBindingManifest("managed-binding", "viewer", "alice")
	out := roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, in), f.token, nil, ""), 201)
	if store.ManagedBy(&out) != store.ManagerAPI {
		t.Fatal("incorrect manager")
	}
	localUsersFresh(t, f)
	read := roleBindingResponse(t, f.call(t, "GET", "/iam/rolebindings/apps/managed-binding", "", f.token, nil, ""), 200)
	if read.UID != out.UID || read.ResourceVersion != out.ResourceVersion {
		t.Fatal("read changed review")
	}
	before := roleBindingImage(t, f.s.opts.Store)
	unchanged := roleBindingResponse(t, f.call(t, "PUT", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, api.ReplaceRoleBindingCommand{RoleBindingReview: roleBindingReview(read), Binding: in}), f.token, nil, ""), 200)
	if unchanged.UID != read.UID || unchanged.ResourceVersion != read.ResourceVersion {
		t.Fatal("unchanged replacement rewrote identity")
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, in), f.token, nil, ""), 409)
	for _, review := range []api.RoleBindingReview{{UID: "other", ResourceVersion: out.ResourceVersion}, {UID: string(out.UID), ResourceVersion: "stale"}} {
		expectServingHTTPStatus(t, f.call(t, "PUT", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, api.ReplaceRoleBindingCommand{RoleBindingReview: review, Binding: in}), f.token, nil, ""), 409)
		expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, review), f.token, nil, ""), 409)
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
	in.Spec.Subjects = []api.BindingSubject{{Kind: "User", Name: "bob"}}
	next := roleBindingResponse(t, f.call(t, "PUT", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, api.ReplaceRoleBindingCommand{RoleBindingReview: roleBindingReview(out), Binding: in}), f.token, nil, ""), 200)
	if next.UID != out.UID || next.ResourceVersion == out.ResourceVersion {
		t.Fatal("replacement lost independent identity or CAS")
	}
	state := localUsersState(t, f.s.opts.Store)
	if len(state.GrantFloors) != 1 || state.GrantFloors[0].Grant != "viewer" || state.GrantFloors[0].ValidAfter.IsZero() {
		t.Fatal("subject removal omitted durable floor")
	}
	localUsersFresh(t, f)
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, roleBindingReview(out)), f.token, nil, ""), 409)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, roleBindingReview(next)), f.token, nil, ""), 204)
	localUsersFresh(t, f)
	expectServingHTTPStatus(t, f.call(t, "GET", "/iam/rolebindings/apps/managed-binding", "", f.token, nil, ""), 404)
	regrant := roleBindingResponse(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, in), f.token, nil, ""), 201)
	if regrant.UID == next.UID {
		t.Fatal("delete/recreate reused incarnation")
	}
	localUsersFresh(t, f)
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/managed-binding", roleBindingBody(t, roleBindingReview(next)), f.token, nil, ""), 409)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	if len(localUsersState(t, f.s.opts.Store).GrantFloors) != 1 {
		t.Fatal("regrant erased floor")
	}
}

func TestServingHTTPRoleBindingsStrictInputOwnershipAndAliases(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	in := roleBindingManifest("strict-binding", "viewer", "alice")
	valid := roleBindingBody(t, in)
	before := roleBindingImage(t, f.s.opts.Store)
	for _, body := range []string{`null`, `[]`, valid + `{}`, strings.Replace(valid, `"role":"viewer"`, `"role":"admin","role":"viewer"`, 1), strings.Replace(valid, `"metadata":{`, `"metadata":{"uid":"invented",`, 1), strings.Replace(valid, `"spec":{`, `"spec":{"private":true,`, 1), strings.Replace(valid, `"kind":"RoleBinding"`, `"kind":"LocalUser"`, 1), strings.Replace(valid, `"role":"viewer"`, `"role":"invented"`, 1), strings.Replace(valid, `"role":"viewer"`, `"role":"zone-admin"`, 1)} {
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", body, f.token, nil, ""), 400)
	}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", strings.Repeat(" ", 64<<10)+valid, f.token, nil, ""), 413)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings?force=true", valid, f.token, nil, ""), 400)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	obj, err := roleBindingDesired(in)
	if err != nil {
		t.Fatal("manifest invalid")
	}
	var owned publicv1.RoleBinding
	localUsersStage(t, f, func(ctx context.Context, tx *sqlite.CommandTx) error {
		out, err := tx.Apply(ctx, obj, store.ApplyOptions{Manager: store.ManagerFile("bindings.yaml"), CreateOnly: true})
		if err == nil {
			owned = *out.(*publicv1.RoleBinding)
		}
		return err
	})
	before = roleBindingImage(t, f.s.opts.Store)
	for _, method := range []string{"PUT", "DELETE"} {
		body := roleBindingBody(t, roleBindingReview(owned))
		if method == "PUT" {
			body = roleBindingBody(t, api.ReplaceRoleBindingCommand{RoleBindingReview: roleBindingReview(owned), Binding: in})
		}
		expectServingHTTPStatus(t, f.call(t, method, "/iam/rolebindings/apps/strict-binding", body, f.token, nil, ""), 409)
	}
	for _, body := range []string{`{}`, `{"uid":"x","uid":"y","resourceVersion":"1"}`, `{"uid":null,"resourceVersion":"1"}`, `{"uid":"x","resourceVersion":"1","force":true}`} {
		expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/strict-binding", body, f.token, nil, ""), 400)
	}
	wrong := in
	wrong.Metadata.Name = "other"
	expectServingHTTPStatus(t, f.call(t, "PUT", "/iam/rolebindings/apps/strict-binding", roleBindingBody(t, api.ReplaceRoleBindingCommand{RoleBindingReview: roleBindingReview(owned), Binding: wrong}), f.token, nil, ""), 400)
	reserved := in
	reserved.Metadata.Annotations = map[string]string{publicv1.ManagedByAnnotation: store.ManagerAPI}
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", roleBindingBody(t, reserved), f.token, nil, ""), 400)
	for _, path := range []string{"/objects/rolebindings/apps/strict-binding", "/objects/RoleBinding/apps/strict-binding", "/rolebindings/apps/strict-binding"} {
		for _, method := range []string{"PUT", "DELETE"} {
			w := f.call(t, method, path, valid, f.token, nil, "")
			if w.Code < 400 {
				t.Fatal("generic alias accepted IAM mutation")
			}
		}
	}
	// Bulk apply must retain the same IAM-kind refusal.
	w := f.call(t, "POST", "/apply", `[`+valid+`]`, f.token, nil, "")
	expectServingHTTPStatus(t, w, http.StatusOK)
	var applied struct {
		Items []api.ApplyResult `json:"items"`
	}
	if json.Unmarshal(w.Body.Bytes(), &applied) != nil || len(applied.Items) != 1 || applied.Items[0].Kind != "RoleBinding" || applied.Items[0].Result != api.ResultError || applied.Items[0].ErrorKey != api.KeyInvalid || applied.Items[0].ResourceVersion != "" {
		t.Fatal("generic apply did not explicitly refuse the IAM document")
	}
	roleBindingUnchanged(t, f.s.opts.Store, before)
}

func TestServingHTTPRoleBindingsPermissionCSRFAndWriterRefusals(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	cookie, info := f.login(t)
	body := roleBindingBody(t, roleBindingManifest("permission-binding", "viewer", "alice"))
	before := roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", body, "", cookie, "wrong"), 403)
	roleBindingUnchanged(t, f.s.opts.Store, before)
	for _, scope := range []string{"viewer", "zone-admin@zone:home"} {
		w := f.call(t, "POST", "/auth/tokens", `{"scopes":["`+scope+`"]}`, "", cookie, info.CSRF)
		expectServingHTTPStatus(t, w, 201)
		var token api.CreatedToken
		if json.Unmarshal(w.Body.Bytes(), &token) != nil {
			t.Fatal("token response")
		}
		localUsersFresh(t, f)
		before = roleBindingImage(t, f.s.opts.Store)
		expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", body, token.Token, nil, ""), 403)
		expectServingHTTPStatus(t, f.call(t, "DELETE", "/iam/rolebindings/apps/x", `{"uid":"x","resourceVersion":"1"}`, token.Token, nil, ""), 403)
		roleBindingUnchanged(t, f.s.opts.Store, before)
	}
	f.stop()
	before = roleBindingImage(t, f.s.opts.Store)
	expectServingHTTPStatus(t, f.call(t, "POST", "/iam/rolebindings", body, f.token, nil, ""), 503)
	roleBindingUnchanged(t, f.s.opts.Store, before)
}

func TestServingHTTPRoleBindingsOriginalFinalClockAndCancellation(t *testing.T) {
	for _, mode := range []string{"expiry", "rollback", "cancel"} {
		for _, changed := range []bool{false, true} {
			name := mode + "/noop"
			if changed {
				name = mode + "/changed"
			}
			t.Run(name, func(t *testing.T) {
				f := newServingCommandFixture(t)
				h, err := newServingHTTP(servingHTTPOptions{Runtime: f.s, Hasher: users.NewHasher(1)})
				if err != nil {
					t.Fatal("handler")
				}
				desired, err := roleBindingDesired(roleBindingManifest("final-binding", "viewer", "alice"))
				if err != nil {
					t.Fatal("manifest")
				}
				before := roleBindingImage(t, f.s.opts.Store)
				called := false
				f.http.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
					ctx, cancel := context.WithCancel(r.Context())
					defer cancel()
					r = r.WithContext(ctx)
					err := h.roleBindingCommand(r, store.KeyOf(desired), "write", func(ctx context.Context, tx *sqlite.CommandTx, observed time.Time) error {
						called = true
						if !observed.Equal(f.now) {
							t.Error("floor did not use original verification time")
						}
						if changed {
							if _, err := stageRoleBindingMutation(ctx, tx, "POST", store.KeyOf(desired), desired, api.RoleBindingReview{}, observed); err != nil {
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
	var released *http.Request
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { released = r })).ServeHTTP(httptest.NewRecorder(), f.request())
	if released == nil {
		t.Fatal("original request missing")
	}
	called := false
	err := f.h.roleBindingCommand(released, store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "released"}, "write", func(context.Context, *sqlite.CommandTx, time.Time) error { called = true; return nil })
	if err == nil || called {
		t.Fatal("released original proof was repaired")
	}
	var absent publicv1.RoleBinding
	if err := f.s.opts.Store.Get(t.Context(), store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "released"}, &absent); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("released command mutated")
	}
}

func TestServingHTTPRoleBindingsRetainOriginalPublication(t *testing.T) {
	f := newServingHTTPFixture(t, false)
	desired, err := roleBindingDesired(roleBindingManifest("stale-publication", "viewer", "alice"))
	if err != nil {
		t.Fatal("manifest")
	}
	called := false
	f.h.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		// Advance the actual source after the original HTTP capture. No new
		// request/publication may rescue this exact already captured command.
		err := f.s.opts.Store.Command(t.Context(), f.s.opts.Store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := tx.Apply(ctx, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "independent-source", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "User", Name: "other"}}}}, store.ApplyOptions{Manager: store.ManagerAPI, CreateOnly: true})
			return err
		})
		if err != nil {
			t.Fatal("independent source command")
		}
		before := roleBindingImage(t, f.s.opts.Store)
		err = f.h.roleBindingCommand(r, store.KeyOf(desired), "write", func(context.Context, *sqlite.CommandTx, time.Time) error { called = true; return nil })
		if err == nil || called {
			t.Fatal("stale original publication was replaced")
		}
		roleBindingUnchanged(t, f.s.opts.Store, before)
	})).ServeHTTP(httptest.NewRecorder(), f.request())
}
