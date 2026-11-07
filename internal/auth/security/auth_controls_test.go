package security

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestAuthControlsInstalledLocalAndExternalIssuance(t *testing.T) {
	t.Run("local-policy", func(t *testing.T) {
		f := newSessionFixture(t)
		deps := loginDependencies(t, f)
		host, err := config.FromFlat(config.Defaults())
		if err != nil {
			t.Fatal(err)
		}
		host.Auth.LocalLogin = config.LocalLoginDisabled
		host.EnvLocked = []string{"auth.localLogin"}
		deps.AuthOverrides = config.CaptureAuthOverrides(host)
		before := ownerState(t, f.st)
		err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}, f.now.Add(time.Second))
			return err
		})
		if !errors.Is(err, servingauth.ErrDenied) || len(ownerState(t, f.st).Sessions) != len(before.Sessions) {
			t.Fatal("current local enrollment ignored installed host policy")
		}
	})
	t.Run("external-duration", func(t *testing.T) {
		f := newExternalIssueFixture(t, false)
		host, err := config.FromFlat(config.Defaults())
		if err != nil {
			t.Fatal(err)
		}
		host.Auth.Session.IdleTimeout = config.Duration(2 * time.Minute)
		host.Auth.Session.Lifetime = config.Duration(7 * time.Minute)
		host.EnvLocked = []string{"auth.session.idleTimeout", "auth.session.lifetime"}
		f.deps.AuthOverrides = config.CaptureAuthOverrides(host)
		proof := f.proof(t, f.now, "alice", "ops")
		var enrolled Enrollment
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var err error
			enrolled, err = IssueExternal(ctx, tx, f.deps, proof, func() time.Time { return f.now.Add(time.Second) })
			return err
		})
		_, session, err := enrolled.Seal(f.view(t), f.now.Add(2*time.Second))
		if err != nil || session.ExpiresAt.Sub(f.now.Add(time.Second)) != 7*time.Minute || session.IdleExpiresAt.Sub(f.now.Add(time.Second)) != 2*time.Minute {
			t.Fatal("external enrollment ignored installed session locks")
		}
	})
}
