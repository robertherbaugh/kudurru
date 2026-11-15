package providercoordinator

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	privatev1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestMemberCommandOutputRetainsVerifiedCredentialExpiry(t *testing.T) {
	for _, mode := range []string{"before-output", "blocked-socket"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberOutputFixture(t)
			id, ok := tokens.Parse(f.wire)
			if !ok {
				t.Fatal("fixture token has no identifier")
			}
			expires := time.Now().Truncate(time.Second).Add(3 * time.Second)
			command(t, f.store, func(ctx context.Context, tx *sqlite.CommandTx) error {
				var token privatev1.APIToken
				if err := tx.Get(ctx, store.Key{Kind: "APIToken", Name: id}, &token); err != nil {
					return err
				}
				token.Spec.ExpiresAt = &metav1.Time{Time: expires}
				_, err := tx.Apply(ctx, &token, store.ApplyOptions{Manager: store.ManagerSystem})
				return err
			})
			ctx, target, request := f.request(t)
			server, client := net.Pipe()
			defer func() { _ = server.Close(); _ = client.Close() }()
			var lifetime *servingauth.CredentialLifetime
			now := time.Now()
			clock := func() time.Time {
				if mode == "blocked-socket" {
					return time.Now()
				}
				return now
			}
			emitted := false
			committed, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
				_, _, verified, err := servingauth.ReauthorizeCurrentForOutput(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, rbac.Attributes{Resource: "stats", Verb: "read"}, clock)
				if err != nil {
					return err
				}
				lifetime = verified
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "credential-output", Data: []byte(`{"committed":true}`)})
			}, func(output context.Context) error {
				if mode == "before-output" {
					now = expires
				}
				return lifetime.WithOutput(output, func(bounded context.Context) error {
					emitted = true
					if err := request.CheckCommandOutput(bounded); err != nil {
						return err
					}
					deadline, finite := bounded.Deadline()
					if !finite || !deadline.Equal(expires) {
						t.Error("original verified token deadline not retained")
						return ErrInvalid
					}
					if err := server.SetWriteDeadline(deadline); err != nil {
						return err
					}
					_, err := server.Write([]byte("protected acknowledgment"))
					return err
				})
			})
			if !committed || err == nil {
				t.Fatal("credential expiry obscured completed command", committed, err)
			}
			if mode == "before-output" {
				if emitted || !errors.Is(err, servingauth.ErrCredentials) {
					t.Fatal("expired credential reached emission", emitted, err)
				}
			} else {
				var timeout net.Error
				if !emitted || !errors.As(err, &timeout) || !timeout.Timeout() {
					t.Fatal("actual socket did not retain verified expiration", emitted, err)
				}
			}
			image, err := f.store.ReadGeneration(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, row := range image.Payload.Dedup {
				found = found || row.Key == "credential-output"
			}
			if !found {
				t.Fatal("output failure erased committed effect")
			}
		})
	}
}
