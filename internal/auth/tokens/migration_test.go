package tokens

import (
	"context"
	"reflect"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func TestMigratedTokenRequiresCurrentIdentityAndExactPrivateProof(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*internalv1.APIToken, *internalv1.ServiceAccount, *MigratedIdentity)
		allowed bool
	}{
		{"public creation after old token", nil, true},
		{"account replaced", func(_ *internalv1.APIToken, s *internalv1.ServiceAccount, _ *MigratedIdentity) { s.UID = "recreated" }, false},
		{"account disabled", func(_ *internalv1.APIToken, s *internalv1.ServiceAccount, _ *MigratedIdentity) {
			s.Spec.Disabled = true
		}, false},
		{"token replaced", func(token *internalv1.APIToken, _ *internalv1.ServiceAccount, _ *MigratedIdentity) {
			token.UID = "recreated-token"
		}, false},
		{"token spec changed", func(token *internalv1.APIToken, _ *internalv1.ServiceAccount, _ *MigratedIdentity) {
			token.Spec.Scopes = []string{"admin"}
		}, false},
		{"token expired", func(token *internalv1.APIToken, _ *internalv1.ServiceAccount, _ *MigratedIdentity) {
			token.Spec.ExpiresAt = &metav1.Time{Time: time.Unix(250, 0)}
		}, false},
		{"original account floor", func(_ *internalv1.APIToken, _ *internalv1.ServiceAccount, b *MigratedIdentity) {
			b.OriginalCreatedAt = time.Unix(150, 0)
		}, false},
		{"absent provenance floor", func(_ *internalv1.APIToken, _ *internalv1.ServiceAccount, b *MigratedIdentity) {
			b.OriginalCreatedAt = time.Time{}
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, id := Generate()
			sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "ci", Namespace: "dns", UID: "new-public-uid", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Unix(200, 0))}}
			token := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, UID: "original-token-uid", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Unix(100, 0))}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "ServiceAccount", Name: "ci", Namespace: "dns", UID: "old-secret-uid"}, SecretHash: Hash(wire), Scopes: []string{"viewer"}}}
			original := token.DeepCopy()
			binding := MigratedIdentity{AccountUID: string(sa.UID), TokenUID: string(token.UID), OriginalCreatedAt: time.Unix(50, 0)}
			if tc.mutate != nil {
				tc.mutate(token, sa, &binding)
			}
			marker := new(int)
			ctx := context.WithValue(t.Context(), marker, "captured-provenance")
			resolver := func(c context.Context, current *internalv1.APIToken, account store.Object) (MigratedIdentity, bool) {
				if c.Value(marker) != "captured-provenance" {
					t.Fatal("migration lost request capture")
				}
				if account.GetName() != "ci" || account.GetNamespace() != "dns" {
					t.Fatal("wrong current account")
				}
				return binding, current.UID == original.UID && reflect.DeepEqual(current.Spec, original.Spec)
			}
			tk, err := New(Options{Store: authtest.Memory(t), Now: func() time.Time { return time.Unix(300, 0) }, Migration: resolver})
			if err != nil {
				t.Fatal(err)
			}
			tk.toks.Upsert(token)
			tk.sas.Upsert(sa)
			p, err := tk.Verify(ctx, wire, "")
			if (err == nil) != tc.allowed {
				t.Fatalf("authentication success=%v, want %v", err == nil, tc.allowed)
			}
			if tc.allowed && (p.AccountUID != "new-public-uid" || p.BuiltinAdmin || !reflect.DeepEqual(p.Scopes, []string{"viewer"}) || !p.CredentialIssuedAt.Equal(time.Unix(100, 0))) {
				t.Fatal("migration changed rights or lost original issuance")
			}
		})
	}
}

func TestMigrationDoesNotBypassWireProofOrRevocation(t *testing.T) {
	tk, _ := New(Options{Store: authtest.Memory(t), Migration: func(context.Context, *internalv1.APIToken, store.Object) (MigratedIdentity, bool) {
		t.Fatal("invalid credential reached migration resolver")
		return MigratedIdentity{}, false
	}})
	wire, id := Generate()
	token := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, UID: types.UID("token"), ResourceVersion: "1"}, Spec: internalv1.APITokenSpec{Owner: internalv1.TokenOwner{Kind: "ServiceAccount", Name: "ci", Namespace: "dns"}, SecretHash: Hash(wire + "wrong")}}
	tk.toks.Upsert(token)
	if _, err := tk.Verify(t.Context(), wire, ""); err == nil {
		t.Fatal("wrong wire proof accepted")
	}
	tk.toks.Remove(store.KeyOf(token))
	if _, err := tk.Verify(t.Context(), wire, ""); err == nil {
		t.Fatal("revoked token accepted")
	}
}

func TestLocalMigrationRequiresUnchangedUserIncarnation(t *testing.T) {
	for _, mode := range []string{"valid", "disabled", "replaced", "changed creation", "no proof"} {
		t.Run(mode, func(t *testing.T) {
			user := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "alice", UID: "original-user", CreationTimestamp: metav1.NewTime(time.Unix(50, 0))}}
			wire, id := Generate()
			token := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, UID: "original-token", ResourceVersion: "1", CreationTimestamp: metav1.NewTime(time.Unix(100, 0))}, Spec: internalv1.APITokenSpec{Owner: alice(), SecretHash: Hash(wire)}}
			binding := MigratedIdentity{AccountUID: string(user.UID), TokenUID: string(token.UID), OriginalCreatedAt: user.CreationTimestamp.Time}
			switch mode {
			case "disabled":
				user.Spec.Disabled = true
			case "replaced":
				user.UID = "replacement"
			case "changed creation":
				user.CreationTimestamp = metav1.NewTime(time.Unix(90, 0))
			}
			tk, err := New(Options{Store: authtest.Memory(t), LocalUser: func(context.Context, string) (*internalv1.LocalUser, bool) { return user, true }, Migration: func(context.Context, *internalv1.APIToken, store.Object) (MigratedIdentity, bool) {
				return binding, mode != "no proof"
			}})
			if err != nil {
				t.Fatal(err)
			}
			tk.toks.Upsert(token)
			p, err := tk.Verify(t.Context(), wire, "")
			if (err == nil) != (mode == "valid") {
				t.Fatalf("authentication success=%v for %s", err == nil, mode)
			}
			if err == nil && (p.AccountUID != string(user.UID) || p.BuiltinAdmin || p.ID != "local:alice") {
				t.Fatal("migration changed local identity or rights")
			}
		})
	}
}
