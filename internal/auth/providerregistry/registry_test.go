package providerregistry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	pr "github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var validatorOnce sync.Once
var validation *validator.Validator
var validationErr error

func open(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	validatorOnce.Do(func() { validation, validationErr = validator.New() })
	if validationErr != nil {
		t.Fatal(validationErr)
	}
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	private := []store.Kind{}
	for _, k := range runtimekinds.Private() {
		private = append(private, store.Kind(k))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, Validator: validation, InternalKinds: private, InitialMode: mode, ValidateReplicaEntry: func(section string, e replication.Entry) error {
		if err := servingauth.ValidateEntry(section, e); err != nil {
			return err
		}
		return pr.ValidateEntry(section, e)
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
func command(t *testing.T, s *sqlite.Store, fn func(context.Context, *sqlite.CommandTx) error) {
	t.Helper()
	if err := s.Command(t.Context(), s.Authority().Term, fn); err != nil {
		t.Fatal(err)
	}
}
func writeAuth(ctx context.Context, tx *sqlite.CommandTx, s servingauth.Security) error {
	rows, err := servingauth.EncodeSecurity(s)
	if err != nil {
		return err
	}
	for _, e := range rows {
		if err := tx.PutSection(ctx, "security", e); err != nil {
			return err
		}
	}
	return nil
}
func newStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "registry.db")
	s := open(t, path, replication.Standalone)
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := writeAuth(ctx, tx, servingauth.EmptySecurity(1)); err != nil {
			return err
		}
		if _, err := tx.Apply(ctx, &publicv1.Kudurru{ObjectMeta: metav1.ObjectMeta{Name: "default"}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		p, err := pr.PlanInitialize(tx.Authority().ClusterID)
		if err != nil {
			return err
		}
		return p.Stage(ctx, tx)
	})
	return s, path
}
func registry(t *testing.T, s *sqlite.Store) *pr.Registry {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	r, err := pr.Decode(i.Payload.Security, i.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}
func spec() *publicv1.IdentityProviderSpec {
	return &publicv1.IdentityProviderSpec{Issuer: "https://issuer.example/realm", ClientID: "browser", ClientSecretRef: &publicv1.CredentialReference{Env: "REGISTRY_TEST_CLIENT"}}
}
func receipt() pr.AuthorityBinding {
	return pr.AuthorityBinding{Profile: "provider-authority/v1", FingerprintKeyID: "key-v1", CredentialFingerprint: strings.Repeat("a", 64), CredentialPolicy: "credentials-v1", NetworkPolicy: "tls-policy-v1", MetadataBinding: "metadata-v1", EntitlementIdentity: "management-v1"}
}
func register(t *testing.T, s *sqlite.Store, id, uid string, at time.Time) *pr.Registry {
	t.Helper()
	r := registry(t, s)
	p, err := pr.PlanReconcile(r, []pr.DesiredConfig{{ID: id, Spec: *spec()}}, at, func() (string, error) { return uid, nil })
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, p.Stage)
	return registry(t, s)
}
func floors(ctx context.Context, tx *sqlite.CommandTx, targets []pr.ProviderIncarnation, at time.Time) error {
	entries, err := tx.ListSections(ctx, "security")
	if err != nil {
		return err
	}
	s, err := servingauth.DecodeSecurity(entries)
	if err != nil {
		return err
	}
	for _, target := range targets {
		found := false
		for i, f := range s.ProviderFloors {
			if f.Provider == target.ID && f.UID == target.UID {
				if f.ValidAfter.Before(at) {
					s.ProviderFloors[i].ValidAfter = at
				}
				found = true
				break
			}
		}
		if !found {
			s.ProviderFloors = append(s.ProviderFloors, servingauth.ProviderFloor{FormatVersion: 1, Provider: target.ID, UID: target.UID, ValidAfter: at})
		}
	}
	s.Epoch++
	return writeAuth(ctx, tx, s)
}

func TestRegistryActualTransactionStableUIDRollbackAndFloorRequired(t *testing.T) {
	s, path := newStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	r := register(t, s, "corp", strings.Repeat("a", 32), at)
	uid, created := r.Sources()[0].UID, r.Sources()[0].CreatedAt
	p, err := pr.PlanReconcile(r, []pr.DesiredConfig{{ID: "corp", Spec: *spec()}}, at.Add(time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.ReadGeneration(t.Context())
	command(t, s, p.Stage)
	after, _ := s.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("same manifest wrote")
	}
	changed := *spec()
	changed.ClientID = "replacement"
	p, err = pr.PlanReconcile(r, []pr.DesiredConfig{{ID: "corp", Spec: changed}}, at.Add(2*time.Second), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Command(t.Context(), s.Authority().Term, p.Stage); !errors.Is(err, pr.ErrFloor) {
		t.Fatal("activation without staged floor", err)
	}
	targets := p.Changed()
	targets[0].UID = "injected"
	abort := errors.New("abort staged registry")
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, p.Changed(), at.Add(2*time.Second)); err != nil {
			return err
		}
		if err := p.Stage(ctx, tx); err != nil {
			return err
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	if registry(t, s).Digest() != r.Digest() {
		t.Fatal("rollback lost registry")
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, p.Changed(), at.Add(2*time.Second)); err != nil {
			return err
		}
		return p.Stage(ctx, tx)
	})
	next := registry(t, s)
	if next.Sources()[0].UID != uid || !next.Sources()[0].CreatedAt.Equal(created) {
		t.Fatal("edit changed incarnation")
	}
	if err := s.Command(t.Context(), s.Authority().Term, p.Stage); !errors.Is(err, pr.ErrConflict) {
		t.Fatal("stale plan admitted", err)
	}
	output := next.ConfigSources()
	output[0].Spec.ClientID = "injected"
	output[0].Spec.ClientSecretRef.Env = "INJECTED"
	if next.ConfigSources()[0].Spec.ClientID != "replacement" {
		t.Fatal("alias")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, replication.Standalone)
	if registry(t, s).Digest() != next.Digest() {
		t.Fatal("restart changed identity")
	}
}

func TestBothOriginAuthorityStageExactUIDSpecAndMaterialFloors(t *testing.T) {
	s, _ := newStore(t)
	at := time.Now().UTC().Add(-time.Minute)
	r := register(t, s, "corp", strings.Repeat("a", 32), at)
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		other := *spec()
		other.Issuer = "https://object.example/issuer"
		_, err := tx.Apply(ctx, &publicv1.IdentityProvider{ObjectMeta: metav1.ObjectMeta{Name: "object"}, Spec: other}, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	var sources []servingauth.ProviderDescriptor
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		sources, err = servingauth.ProviderSourcesCurrent(ctx, tx, r.ConfigSources())
		return err
	})
	desired := []pr.DesiredAuthority{}
	for _, d := range sources {
		desired = append(desired, pr.DesiredAuthority{Source: d, Binding: receipt()})
	}
	p, err := pr.PlanAcceptAuthority(r, desired, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	command(t, s, p.Stage)
	r = registry(t, s)
	if len(r.Authorities()) != 2 || r.Sources()[0].AuthorityKey == "" {
		t.Fatal("missing both-origin inventory")
	}
	// Even an unchanged receipt plan rechecks the actual transaction source.
	p, err = pr.PlanAcceptAuthority(r, desired, at.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	abort := errors.New("rollback object spec")
	if err := s.Command(t.Context(), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var obj publicv1.IdentityProvider
		if err := tx.Get(ctx, store.Key{Kind: "IdentityProvider", Name: "object"}, &obj); err != nil {
			return err
		}
		obj.Spec.ClientID = "replacement"
		if _, err := tx.Apply(ctx, &obj, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			return err
		}
		if err := p.Stage(ctx, tx); !errors.Is(err, pr.ErrConflict) {
			t.Fatal("stale object authority admitted", err)
		}
		return abort
	}); !errors.Is(err, abort) {
		t.Fatal(err)
	}
	desired[0].Binding.CredentialFingerprint = strings.Repeat("b", 64)
	p, err = pr.PlanAcceptAuthority(r, desired, at.Add(3*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Command(t.Context(), s.Authority().Term, p.Stage); !errors.Is(err, pr.ErrFloor) {
		t.Fatal("material change lacks floor", err)
	}
	command(t, s, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := floors(ctx, tx, p.Changed(), at.Add(3*time.Second)); err != nil {
			return err
		}
		return p.Stage(ctx, tx)
	})
	if registry(t, s).Epoch() != r.Epoch()+1 {
		t.Fatal("epoch advanced more than once")
	}
}

func TestRegistryStrictManifestBoundsVersionsAndOverflow(t *testing.T) {
	s, _ := newStore(t)
	r := register(t, s, "corp", strings.Repeat("a", 32), time.Now().UTC())
	rows, err := pr.Encode(r)
	if err != nil {
		t.Fatal(err)
	}
	image, _ := s.ReadGeneration(t.Context())
	cluster := image.Term.ClusterID
	for _, tc := range []struct {
		name   string
		change func([]replication.Entry) []replication.Entry
	}{
		{"missing-root", func(e []replication.Entry) []replication.Entry {
			return slices.DeleteFunc(e, func(e replication.Entry) bool { return e.Key == pr.RootKey })
		}},
		{"duplicate-row", func(e []replication.Entry) []replication.Entry { return append(e, e[0]) }},
		{"null-inventory", func(e []replication.Entry) []replication.Entry {
			for i := range e {
				if e[i].Key == pr.RootKey {
					e[i].Data = bytes.Replace(e[i].Data, []byte(`"authorities":[]`), []byte(`"authorities":null`), 1)
				}
			}
			return e
		}},
		{"case-alias", func(e []replication.Entry) []replication.Entry {
			e[0].Data = bytes.Replace(e[0].Data, []byte(`"active"`), []byte(`"Active"`), 1)
			return e
		}},
		{"absent-boolean", func(e []replication.Entry) []replication.Entry {
			e[0].Data = bytes.Replace(e[0].Data, []byte(`"active":true,`), nil, 1)
			return e
		}},
		{"duplicate-json", func(e []replication.Entry) []replication.Entry {
			e[0].Data = bytes.Replace(e[0].Data, []byte(`"active":true`), []byte(`"active":true,"active":false`), 1)
			return e
		}},
		{"unknown-version", func(e []replication.Entry) []replication.Entry {
			for i := range e {
				e[i].Data = bytes.Replace(e[i].Data, []byte(`"formatVersion":1`), []byte(`"formatVersion":2`), 1)
			}
			return e
		}},
		{"row-byte-cap", func(e []replication.Entry) []replication.Entry {
			e[0].Data = bytes.Repeat([]byte(" "), pr.MaxRowBytes+1)
			return e
		}},
		{"invalid-utf8", func(e []replication.Entry) []replication.Entry {
			e[0].Data = []byte{'{', '"', 'x', '"', ':', '"', 0xff, '"', '}'}
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := make([]replication.Entry, len(rows))
			for i, x := range rows {
				e[i] = x
				e[i].Data = bytes.Clone(x.Data)
			}
			if _, err := pr.Decode(tc.change(e), cluster); err == nil {
				t.Fatal("unsafe manifest accepted")
			}
		})
	}
	if _, err := pr.Decode(rows, "other-cluster"); err == nil {
		t.Fatal("cluster mismatch")
	}
	if _, err := pr.PlanReconcile(r, make([]pr.DesiredConfig, pr.MaxActiveSources+1), time.Now(), nil); !errors.Is(err, pr.ErrBounds) {
		t.Fatal("count cap", err)
	}
	big := *spec()
	big.DisplayName = strings.Repeat("x", 1<<20)
	if _, err := pr.PlanReconcile(r, []pr.DesiredConfig{{ID: "corp", Spec: big}}, time.Now(), nil); !errors.Is(err, pr.ErrBounds) {
		t.Fatal("preallocation cap", err)
	}
	for i := range rows {
		if rows[i].Key == pr.RootKey {
			var root pr.Root
			_ = json.Unmarshal(rows[i].Data, &root)
			root.Epoch = math.MaxUint64
			rows[i].Data, _ = json.Marshal(root)
		}
	}
	maxEpoch, err := pr.Decode(rows, cluster)
	if err != nil {
		t.Fatal(err)
	}
	changed := *spec()
	changed.ClientID = "new"
	if _, err := pr.PlanReconcile(maxEpoch, []pr.DesiredConfig{{ID: "corp", Spec: changed}}, time.Now(), nil); !errors.Is(err, pr.ErrBounds) {
		t.Fatal("overflow", err)
	}
	if fmt.Sprintf("%#v", r) != "private provider registry" {
		t.Fatal("diagnostic leaks registry")
	}
}
