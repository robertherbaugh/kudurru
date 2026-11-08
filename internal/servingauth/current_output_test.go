package servingauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCurrentCredentialOutputPreservesEveryVerifiedExpiry(t *testing.T) {
	for _, kind := range []string{"sa-token", "local-token", "cookie-idle", "cookie-absolute", "jwt", "external-token", "external-cookie-idle", "external-cookie-absolute"} {
		t.Run(kind, func(t *testing.T) {
			f, raw, expires := clockProof(t, kind)
			now := f.now
			var lifetime *CredentialLifetime
			err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, verified, err := ReauthorizeCurrentForOutput(ctx, tx, f.deps, raw, readAttrs(), func() time.Time { return now })
				if err != nil {
					return err
				}
				lifetime = verified
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "output-committed", Data: []byte(`{"committed":true}`)})
			})
			if err != nil || lifetime == nil {
				t.Fatal("current raw verification failed", err)
			}
			committed := f.image(t)
			original, cancel := context.WithDeadline(t.Context(), expires.Add(time.Minute))
			defer cancel()
			var escaped context.Context
			if err := lifetime.WithOutput(original, func(output context.Context) error {
				escaped = output
				if deadline, ok := output.Deadline(); !ok || !deadline.Equal(expires) {
					t.Fatal("verified credential expiry lost", deadline, expires)
				}
				return nil
			}); err != nil {
				t.Fatal("valid original output refused", err)
			}
			if !errors.Is(escaped.Err(), context.Canceled) {
				t.Fatal("output context escaped synchronous scope")
			}
			shorter, stop := context.WithTimeout(original, time.Second)
			defer stop()
			want, _ := shorter.Deadline()
			if err := lifetime.WithOutput(shorter, func(output context.Context) error {
				if bound, _ := output.Deadline(); !bound.Equal(want) {
					t.Fatal("credential extended original output bound")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			// Expiry during emission is reported independently of the already
			// committed effect. No claim is made that handed-off bytes roll back.
			if err := lifetime.WithOutput(original, func(context.Context) error {
				now = expires
				return nil
			}); !errors.Is(err, ErrCredentials) {
				t.Fatal("expiry during output was hidden", err)
			}
			entered := false
			if err := lifetime.WithOutput(original, func(context.Context) error { entered = true; return nil }); !errors.Is(err, ErrCredentials) || entered {
				t.Fatal("expired proof reached output", entered, err)
			}
			now = f.now.Add(-time.Nanosecond)
			if err := lifetime.WithOutput(original, func(context.Context) error { entered = true; return nil }); !errors.Is(err, ErrCredentials) || entered {
				t.Fatal("clock regression reached output", entered, err)
			}
			now = f.now
			cancel()
			if err := lifetime.WithOutput(original, func(context.Context) error { entered = true; return nil }); !errors.Is(err, context.Canceled) || entered {
				t.Fatal("original cancellation detached", entered, err)
			}
			after := f.image(t)
			if !committed.Generation().Same(after.Generation()) || !reflect.DeepEqual(committed.Payload, after.Payload) {
				t.Fatal("output refusal changed actual committed state")
			}
			wire, err := json.Marshal(lifetime)
			if err != nil || strings.Contains(string(wire), raw.Value) || strings.Contains(fmt.Sprintf("%#v", lifetime), raw.Value) {
				t.Fatal("lifetime leaked raw proof", err)
			}
		})
	}
}

func TestCurrentCredentialOutputRequiresVerifiedFiniteInput(t *testing.T) {
	f := newFixture(t)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	for _, lifetime := range []*CredentialLifetime{nil, {}} {
		if err := lifetime.WithOutput(ctx, func(context.Context) error { t.Fatal("unverified output"); return nil }); err == nil {
			t.Fatal("unverified lifetime admitted")
		}
	}
	var lifetime *CredentialLifetime
	if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, verified, err := ReauthorizeCurrentForOutput(ctx, tx, f.deps, Credential{Kind: APIToken, Value: f.wire}, readAttrs(), nil)
		lifetime = verified
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := lifetime.WithOutput(context.Background(), func(context.Context) error { t.Fatal("unbounded output"); return nil }); !errors.Is(err, ErrInvalid) {
		t.Fatal("unbounded original input accepted", err)
	}
	if err := lifetime.WithOutput(ctx, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil output accepted", err)
	}
	for _, kind := range []CredentialKind{Password, "kubernetes", ""} {
		if _, proof, err := ReauthorizeCurrentForOutput(ctx, nil, f.deps, Credential{Kind: kind}, readAttrs(), nil); !errors.Is(err, ErrUnsupported) || proof != nil {
			t.Fatal("unsupported proof gained output lifetime", err)
		}
	}
	want, _ := ctx.Deadline()
	if err := lifetime.WithOutput(ctx, func(output context.Context) error {
		if bound, _ := output.Deadline(); !bound.Equal(want) {
			t.Fatal("nonexpiring API token replaced original finite bound")
		}
		cancel()
		return nil
	}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation during output hidden", err)
	}
}
