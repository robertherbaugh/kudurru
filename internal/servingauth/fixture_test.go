package servingauth

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var valOnce sync.Once
var val *validator.Validator
var valErr error

func openStore(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	valOnce.Do(func() { val, valErr = validator.New() })
	if valErr != nil {
		t.Fatal(valErr)
	}
	scheme, err := internalv1.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := make([]store.Kind, len(internalv1.Kinds))
	for i, k := range internalv1.Kinds {
		private[i] = store.Kind(k)
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, Validator: val, InitialMode: mode, Identity: "auth-test", ValidateReplicaEntry: ValidateEntry})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

type fixture struct {
	st                    *sqlite.Store
	path                  string
	registry              Schema
	deps                  Dependencies
	now                   time.Time
	user                  *internalv1.LocalUser
	sa                    *publicv1.ServiceAccount
	token                 *internalv1.APIToken
	wire, localWire, csrf string
	security              Security
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{path: filepath.Join(t.TempDir(), "state.db"), now: time.Now().UTC().Truncate(time.Second), csrf: "0123456789abcdef0123456789abcdef", security: EmptySecurity(1)}
	f.st = openStore(t, f.path, replication.Standalone)
	hasher := users.NewHasher(1)
	phc, err := hasher.Hash(t.Context(), "correct-password")
	if err != nil {
		t.Fatal(err)
	}
	created := metav1.NewTime(f.now.Add(-time.Hour))
	user := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", CreationTimestamp: created}, Spec: internalv1.LocalUserSpec{PasswordHash: phc, Groups: []string{"operators"}}}
	f.user = apply(t, f.st, user).(*internalv1.LocalUser)
	sa := &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "apps", CreationTimestamp: created}}
	f.sa = apply(t, f.st, sa).(*publicv1.ServiceAccount)
	apply(t, f.st, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}})
	apply(t, f.st, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "test-idp"}, Spec: publicv1.IdentityProviderSpec{Issuer: "https://issuer.example.invalid", ClientID: "fixture", ClientSecretRef: &publicv1.CredentialReference{Env: "SERVINGAUTH_TEST_CLIENT_SECRET"}}})
	apply(t, f.st, &publicv1.RoleBinding{ObjectMeta: metav1.ObjectMeta{Name: "read", Namespace: "apps"}, Spec: publicv1.RoleBindingSpec{Role: "viewer", Subjects: []publicv1.BindingSubject{{Kind: "ServiceAccount", Name: "ci", Namespace: "apps"}, {Kind: "Group", Name: "operators"}}}})
	wire, id := tokens.Generate()
	f.wire = wire
	token := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(-30 * time.Minute))}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "ServiceAccount", Namespace: "apps", Name: "ci", ID: "sa:apps/ci", UID: string(f.sa.UID)}, SecretHash: tokens.Hash(wire), Scopes: []string{"viewer"}}}
	f.token = apply(t, f.st, token).(*internalv1.APIToken)
	wire, id = tokens.Generate()
	f.localWire = wire
	apply(t, f.st, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(-30 * time.Minute))}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "User", Name: "alice", ID: "local:alice", Provider: "local", UID: string(f.user.UID)}, SecretHash: tokens.Hash(wire)}})
	f.security.Sessions = []SessionRecord{{FormatVersion: 1, ID: "abcdefghijklmnop", Account: accountRef(f.user), Provider: "local", Epoch: 1, IssuedAt: f.now.Add(-10 * time.Minute), ExpiresAt: f.now.Add(time.Hour), IdleExpiresAt: f.now.Add(30 * time.Minute), Grants: []string{"viewer"}, CSRFHash: CSRFHash(f.csrf)}}
	f.writeSecurity(t)
	image, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	scheme, _ := internalv1.NewScheme()
	f.registry = Schema{Scheme: scheme, Kinds: slices.Clone(image.KindManifest), SchemaVersion: store.SchemaVersion}
	permissions, err := rbac.LoadPermissions()
	if err != nil {
		t.Fatal(err)
	}
	f.deps = Dependencies{Identity: "host-v1", ExpectedIdentity: "host-v1", License: LicensePolicy{RBAC: true}, Permissions: permissions, CurrentKey: []byte("01234567890123456789012345678901"), Hasher: hasher}
	return f
}
func apply(t *testing.T, s *sqlite.Store, obj store.Object) store.Object {
	t.Helper()
	out, err := s.Apply(t.Context(), obj, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		t.Fatal(err)
	}
	return out
}
func (f *fixture) writeSecurity(t *testing.T) {
	t.Helper()
	rows, err := EncodeSecurity(f.security)
	if err != nil {
		t.Fatal(err)
	}
	changes := make([]sqlite.SectionChange, len(rows))
	for i, row := range rows {
		changes[i] = sqlite.SectionChange{Section: "security", Key: row.Key, Data: row.Data}
	}
	if _, err = f.st.ApplyCluster(t.Context(), nil, store.ApplyOptions{}, changes); err != nil {
		t.Fatal(err)
	}
}
func (f *fixture) image(t *testing.T) *replication.Image {
	t.Helper()
	image, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return image
}
func (f *fixture) view(t *testing.T) *View {
	t.Helper()
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Prepare(t.Context(), cut, f.deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(view.Release)
	return view
}
func readAttrs() rbac.Attributes {
	return rbac.Attributes{Resource: "zones", Verb: "read", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
}
