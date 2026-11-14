package servingauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRealImageImmutableVerificationAfterStoreClose(t *testing.T) {
	f := newFixture(t)
	image := f.image(t)
	cut, err := CaptureImage(t.Context(), image, f.registry)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	defer view.Release()
	// Caller-owned documents and dependency maps cannot mutate the sealed cut.
	image.Payload.Objects = nil
	image.Payload.Security = nil
	f.deps.Permissions.Roles["viewer"] = nil
	f.deps.CurrentKey[0] = 'x'
	if err = f.st.Close(); err != nil {
		t.Fatal(err)
	}
	cookie, err := view.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	for _, proof := range []Credential{{Kind: APIToken, Value: f.wire}, {Kind: APIToken, Value: f.localWire}, {Kind: Cookie, Value: cookie}, {Kind: Password, Username: "ALICE", Value: "correct-password"}} {
		p, err := view.Authenticate(t.Context(), proof, f.now)
		if err != nil {
			t.Fatal(proof.Kind, err)
		}
		d, err := view.Authorize(p, readAttrs(), f.now)
		if err != nil || !d.Allowed {
			t.Fatal(proof.Kind, d, err)
		}
		p.Groups = append(p.Groups, "injected")
		p.Scopes = append(p.Scopes, "admin")
		again, err := view.Authenticate(t.Context(), proof, f.now)
		if err != nil || slices.Contains(again.Groups, "injected") || slices.Contains(again.Scopes, "admin") {
			t.Fatal("output aliases view", err)
		}
	}
	for _, text := range []string{fmt.Sprintf("%#v", view), fmt.Sprintf("%#v", cut)} {
		if text == "" || len(text) > 100 {
			t.Fatal("private projection escaped formatting")
		}
	}
}
func TestCompleteManifestAndDependencyFailures(t *testing.T) {
	f := newFixture(t)
	for _, what := range []string{"root", "iam-kind", "settings", "hash", "owner-version"} {
		t.Run(what, func(t *testing.T) {
			image := f.image(t)
			switch what {
			case "root":
				image.Payload.Security = []replication.Entry{}
			case "iam-kind":
				image.KindManifest = slices.DeleteFunc(image.KindManifest, func(k replication.Kind) bool { return k.Kind == "IdentityProvider" })
			case "settings":
				image.Payload.Objects = slices.DeleteFunc(image.Payload.Objects, func(raw json.RawMessage) bool {
					var doc struct{ Kind string }
					_ = json.Unmarshal(raw, &doc)
					return doc.Kind == "Kudurru"
				})
			case "hash":
				image.PayloadSHA256 = "different"
			case "owner-version":
				for i, row := range image.Payload.Security {
					if row.Key == RootKey {
						var root RootEntry
						_ = json.Unmarshal(row.Data, &root)
						root.FormatVersion = 99
						image.Payload.Security[i].Data, _ = json.Marshal(root)
					}
				}
			}
			if what != "hash" {
				if err := replication.Seal(image); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := CaptureImage(t.Context(), image, f.registry); err == nil {
				t.Fatal("partial/incompatible image accepted")
			}
		})
	}
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	deps := f.deps
	deps.ExpectedIdentity = "new-host-version"
	if _, err = Prepare(t.Context(), cut, deps); !errors.Is(err, ErrDependency) {
		t.Fatal(err)
	}
}
func TestHeldOldAndNewCutsRevocationAndExpiry(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	cookie, err := old.SealSession("abcdefghijklmnop", f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	// Atomic grant/security change removes a role, records the floor, then regrant
	// never revives the old session. Held old requests retain their original cut.
	f.security.Epoch++
	f.security.GrantFloors = []GrantFloor{{FormatVersion: 1, Grant: "viewer", ValidAfter: f.security.Sessions[0].IssuedAt}}
	f.writeSecurity(t)
	current := f.view(t)
	if _, err = old.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); err != nil {
		t.Fatal("held old request changed cut", err)
	}
	if _, err = current.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("equal-time revocation revived cookie", err)
	}
	if old.Base().Same(current.Base()) {
		t.Fatal("security write did not change Base")
	}
	if _, err = old.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.security.Sessions[0].IdleExpiresAt); err == nil {
		t.Fatal("idle expiry renewed by capture")
	}
	if _, err = old.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire + "wrong"}, f.now); err == nil {
		t.Fatal("wrong verifier accepted")
	}
}
func TestPrivateAdminScopesLicenseAndCurrentIdentity(t *testing.T) {
	f := newFixture(t)
	owner := f.token.Spec.Owner
	owner.Provider = "kudurru"
	apply(t, f.st, &internalv1.PrincipalState{ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner)}, Spec: internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true}})
	f.deps.License.RBAC = false
	v := f.view(t)
	p, err := v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now)
	if err != nil || !p.BuiltinAdmin {
		t.Fatal("exact private admin missing", err)
	}
	d, err := v.Authorize(p, readAttrs(), f.now)
	if err != nil || !d.Allowed || d.Path != rbac.PathLocal {
		t.Fatal(d, err)
	}
	attrs := readAttrs()
	attrs.Verb = "write"
	d, err = v.Authorize(p, attrs, f.now)
	if err != nil || d.Allowed {
		t.Fatal("admin bypassed token scope", d, err)
	}
	local, err := v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.localWire}, f.now)
	if err != nil {
		t.Fatal(err)
	}
	d, err = v.Authorize(local, readAttrs(), f.now)
	if err != nil || d.Allowed {
		t.Fatal("unpinned management license allowed bindings", d, err)
	}
	// A newly disabled/recreated account cannot inherit the captured credential.
	disabled := f.sa.DeepCopy()
	disabled.Spec.Disabled = true
	apply(t, f.st, disabled)
	newer := f.view(t)
	if _, err = newer.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err == nil {
		t.Fatal("disabled account accepted")
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err != nil {
		t.Fatal("old request changed captured authority", err)
	}
	if err = f.st.Delete(t.Context(), store.KeyOf(f.sa), store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
		t.Fatal(err)
	}
	replacement := f.sa.DeepCopy()
	replacement.UID = ""
	replacement.ResourceVersion = ""
	replacement.Annotations = nil
	apply(t, f.st, replacement)
	replaced := f.view(t)
	if _, err = replaced.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now); err == nil {
		t.Fatal("recreated account inherited old UID proof")
	}
}
func TestLegacySealedCookieDeniedWithoutEnrollment(t *testing.T) {
	f := newFixture(t)
	v := f.view(t)
	legacy, err := session.NewSealed(session.Options{Key: f.deps.CurrentKey, Now: func() time.Time { return f.now }})
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := legacy.Issue(t.Context(), auth.Session{Principal: auth.Principal{Kind: auth.KindUser, ID: "local:alice", Name: "alice", Provider: "local", AccountUID: string(f.user.UID)}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = v.Authenticate(t.Context(), Credential{Kind: Cookie, Value: cookie}, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("legacy cookie fabricated enrollment", err)
	}
}
