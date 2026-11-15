package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/security"
	"github.com/robertherbaugh/kudurru/internal/objects"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Exercise both physical source layouts through the mounted production loader,
// real SQLite security owner and verified TLS OP. Original proofs are never
// renewed or retried after refusal.
func TestSQLiteObjectSourceActualTLSFrozenGrantsCannotRevive(t *testing.T) {
	for _, inline := range []bool{false, true} {
		name := "directory"
		if inline {
			name = "inline"
		}
		t.Run(name, func(t *testing.T) {
			f := newServingBrowserFixture(t)
			dir := t.TempDir()
			path := filepath.Join(dir, "binding.yaml")
			desired := roleBindingManifest("source-operator", "operator", "corp:alice")
			desired.Spec.Scope.Kind = "cluster"
			write := func(present bool) {
				var value any = desired
				if inline {
					items := []any{}
					if present {
						items = append(items, desired)
					}
					value = map[string]any{"objects": items}
				}
				raw := []byte{}
				if inline || present {
					var err error
					raw, err = json.Marshal(value)
					if err != nil {
						t.Fatal("source encoding")
					}
				}
				if os.WriteFile(path, raw, 0o600) != nil {
					t.Fatal("source write")
				}
			}
			write(true)
			opts := objects.Options{Store: f.s.opts.Store, Validator: f.s.opts.Validator, Dir: dir, PollInterval: -1, RoleBindings: newSQLiteObjectSources(f.s.opts.Store), RequireRoleBindings: true}
			if inline {
				opts.Dir = ""
				opts.InlineFile = path
				opts.InlineSource = "binding.yaml"
			}
			loader, err := objects.New(opts)
			if err != nil {
				t.Fatal("production loader construction")
			}
			// Retain the actual installed writer episode once; never reacquire a
			// successor to repair a refused load.
			original := f.s.primary.writer.active.Load()
			if original == nil {
				t.Fatal("writer missing")
			}
			ctx := sqlite.WithWriterToken(t.Context(), original.token)
			// Approved qualification-only isolation before ANY external enrollment.
			// Use the real existing security helper with this retained original writer.
			if err := f.s.opts.Store.Command(ctx, original.token.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
				var viewer publicv1.RoleBinding
				if err := tx.Get(ctx, store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "browser-view"}, &viewer); err != nil {
					return err
				}
				return security.Delete(ctx, tx, &viewer, store.DeleteOptions{Manager: store.ManagerSystem}, time.Now().UTC())
			}); err != nil {
				t.Fatal("operator isolation setup refused")
			}
			localUsersFresh(t, f.servingHTTPFixture)
			operatorFloors := func() int {
				count := 0
				for _, floor := range f.security(t).GrantFloors {
					if floor.Grant == "operator" {
						count++
					}
				}
				return count
			}
			if operatorFloors() != 0 {
				t.Fatal("setup manufactured operator floor")
			}
			observe := func(phase string, index int, w *httptest.ResponseRecorder, want int) {
				var info struct {
					Roles []string `json:"roles"`
				}
				if w.Code == 200 && json.Unmarshal(w.Body.Bytes(), &info) != nil {
					t.Fatal("safe proof metadata decode")
				}
				operator := false
				for _, role := range info.Roles {
					operator = operator || role == "operator"
				}
				t.Logf("F44 checkpoint=proof phase=%s index=%d status=%d expected=%d operator_role=%t operator_floors=%d", phase, index, w.Code, want, operator, operatorFloors())
				if w.Code != want {
					t.Fatalf("proof status mismatch: got=%d expected=%d", w.Code, want)
				}
				if want == http.StatusOK && !operator {
					t.Fatal("current enrollment lacks source operator permission")
				}
			}
			present := func() bool {
				var binding publicv1.RoleBinding
				return f.s.opts.Store.Get(ctx, store.Key{Kind: "RoleBinding", Namespace: "apps", Name: "source-operator"}, &binding) == nil
			}
			load := func() {
				report := loader.Load(ctx)
				if report.Count(objects.SeverityError) != 0 {
					t.Fatal("production loader refused valid source")
				}
				f.s.publisher.Wake()
				localUsersFresh(t, f.servingHTTPFixture)
			}
			load()
			if operatorFloors() != 0 {
				t.Fatal("initial source created revocation")
			}
			cookie, _, csrf := f.logoutLogin(t)
			created := f.security(t).ExternalIdentities[0].CreatedAt
			awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > created.Unix() })
			issued := f.call(t, "POST", "/auth/tokens", `{}`, "", cookie, csrf)
			expectServingHTTPStatus(t, issued, 201)
			var pat api.CreatedToken
			if json.Unmarshal(issued.Body.Bytes(), &pat) != nil {
				t.Fatal("personal token response")
			}
			localUsersFresh(t, f.servingHTTPFixture)
			observe("initial", 0, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 200)
			observe("initial", 1, f.call(t, "GET", "/auth/me", "", pat.Token, nil, ""), 200)
			t.Log("F44 checkpoint=initial_proofs cookie=200 pat=200")
			write(false)
			load()
			if operatorFloors() != 1 {
				t.Fatal("source removal omitted durable operator floor")
			}
			var floor time.Time
			for _, item := range f.security(t).GrantFloors {
				if item.Grant == "operator" {
					floor = item.ValidAfter
				}
			}
			if floor.IsZero() || present() {
				t.Fatal("source removal/floor was not one committed result")
			}
			observe("removed", 0, f.call(t, "GET", "/auth/me", "", "", cookie, ""), 401)
			observe("removed", 1, f.call(t, "GET", "/auth/me", "", pat.Token, nil, ""), 401)
			t.Logf("F44 checkpoint=removed_binding present=%t", present())
			write(true)
			load()
			if operatorFloors() != 1 || !present() {
				t.Fatal("source regrant erased operator floor or omitted binding")
			}
			t.Logf("F44 checkpoint=regranted_binding present=%t", present())
			// Same-spec regrant must not revive either exact original frozen proof.
			for proofIndex, proof := range []struct {
				token  string
				cookie *http.Cookie
			}{{cookie: cookie}, {token: pat.Token}} {
				w := f.call(t, "GET", "/auth/me", "", proof.token, proof.cookie, "")
				observe("regranted", proofIndex, w, http.StatusUnauthorized)
			}
			// Only a genuinely new OP enrollment after the original floor can receive
			// the regranted permission. Do not manufacture a new iat on the old proof.
			awaitServingRuntime(t, f.s, func() bool { return time.Now().Unix() > floor.Unix() })
			fresh, _, _ := f.logoutLogin(t)
			observe("fresh", 0, f.call(t, "GET", "/auth/me", "", "", fresh, ""), 200)
			sourceReplicaRestart(t, f.s.opts.Store, f.s.opts.Validator)
		})
	}
}
