package tokens

import (
	"context"
	"errors"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/authtest"
	"github.com/robertherbaugh/kudurru/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrivateAuthorityCannotBeClaimedByNameOrAnnotation(t *testing.T) {
	authtest.Drivers(t, func(t *testing.T, st store.Store) {
		ctx := context.Background()
		tk := newTokens(t, st, &clock{t: time.Now()}, 0)
		preexisting := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: BootstrapAccount, Namespace: tk.Namespace(), Annotations: map[string]string{"kudurru.io/builtin-admin": "true"}}}
		if _, err := st.Apply(ctx, preexisting, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
			t.Fatal(err)
		}
		if _, err := tk.EnsureServiceAccount(ctx, tk.Namespace(), BootstrapAccount, "bootstrap", true); !errors.Is(err, ErrBootstrapIdentity) {
			t.Fatal("existing account acquired private authority by name")
		}
		privileged, err := tk.EnsureServiceAccount(ctx, "dns", "trusted", "trusted bootstrap", true)
		if err != nil || !tk.BuiltinAdmin(privileged) {
			t.Fatal("new trusted account lacks private authority")
		}
		if _, err := tk.EnsureServiceAccount(ctx, "dns", "trusted", "retry", true); err != nil {
			t.Fatal(err)
		}
		replaced := privileged.DeepCopy()
		replaced.UID = "replacement-uid"
		if tk.BuiltinAdmin(replaced) {
			t.Fatal("replacement inherited private authority")
		}
		restarted := newTokens(t, st, &clock{t: time.Now()}, 0)
		if !restarted.BuiltinAdmin(privileged) {
			t.Fatal("private authority did not survive cache restart")
		}
	})
}
