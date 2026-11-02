package servingauth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

func TestAuthControlsCapturedCutRestartFollowerAndRevocation(t *testing.T) {
	f := newFixture(t)
	old := f.view(t)
	oldControls, err := old.AuthControls()
	if err != nil {
		t.Fatal(err)
	}
	cookie, err := old.SealSession(f.security.Sessions[0].ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	settings, err := old.Settings()
	if err != nil {
		t.Fatal(err)
	}
	settings.Spec.Settings = map[string]extensions.JSON{"auth": {Raw: []byte(`{"localLogin":"disabled","session":{"idleTimeout":"3m","lifetime":"20m","secure":"always"},"tokens":{"maxLifetime":"1h"}}`)}}
	apply(t, f.st, settings)
	changed := f.view(t)
	controls, err := changed.AuthControls()
	if err != nil || controls.SessionLifetime != 20*time.Minute || controls.SessionSecure != "always" || controls.LocalLogin != config.LocalLoginDisabled {
		t.Fatal("published controls did not follow canonical cut")
	}
	if retained, err := old.AuthControls(); err != nil || retained != oldControls {
		t.Fatal("held auth view changed after successor publication")
	}
	// The new setting cannot shorten, renew or rescue the exact enrolled row.
	session, err := changed.CookieSession(cookie, f.now)
	if err != nil || !session.ExpiresAt.Equal(f.security.Sessions[0].ExpiresAt) || !session.IdleExpiresAt.Equal(f.security.Sessions[0].IdleExpiresAt) {
		t.Fatal("new controls altered original session fields")
	}
	if _, err := changed.CookieSession(cookie, f.security.Sessions[0].IdleExpiresAt); !errors.Is(err, ErrCredentials) {
		t.Fatal("new lifetime rescued original idle deadline")
	}
	if _, err := changed.CookieSession(cookie, f.security.Sessions[0].IssuedAt.Add(-time.Nanosecond)); !errors.Is(err, ErrCredentials) {
		t.Fatal("new controls repaired regressed credential clock")
	}
	if _, err := changed.Authenticate(t.Context(), Credential{Kind: APIToken, Value: f.wire}, f.now.Add(2*time.Hour)); err != nil {
		t.Fatal("new token lifetime cap retroactively changed an existing token")
	}
	f.security.Sessions[0].Revoked = true
	f.writeSecurity(t)
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openStore(t, f.path, replication.Standalone)
	restarted := f.view(t)
	if c, err := restarted.AuthControls(); err != nil || c != controls {
		t.Fatal("durable restart lost canonical controls")
	}
	if _, err := restarted.CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("restart revived revoked session")
	}
	image := f.image(t)
	follower := openStore(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := follower.Enroll(t.Context(), image.Term); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(t.Context(), *image)
	if err != nil {
		t.Fatal(err)
	}
	authority := follower.Authority()
	if _, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed}); err != nil {
		t.Fatal(err)
	}
	f.st = follower
	// Each follower retains its own installed host locks, never the sender's.
	host, err := config.FromFlat(config.Defaults())
	if err != nil {
		t.Fatal(err)
	}
	host.Auth.Session.Secure = "never"
	host.EnvLocked = []string{"auth.session.secure"}
	f.deps.AuthOverrides = config.CaptureAuthOverrides(host)
	followed := f.view(t)
	if c, err := followed.AuthControls(); err != nil || c.SessionSecure != "never" || c.SessionLifetime != controls.SessionLifetime {
		t.Fatal("follower lost canonical controls or its own host lock")
	}
	if followed.DependencyIdentity() == restarted.DependencyIdentity() {
		t.Fatal("installed override missing from dependency identity")
	}
	if _, err := followed.CookieSession(cookie, f.now); !errors.Is(err, ErrCredentials) {
		t.Fatal("replication revived revoked credential")
	}
	called := false
	if err := follower.Command(t.Context(), follower.Authority().Term, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrReadOnly) || called {
		t.Fatal("follower controls granted writer authority")
	}
	// The settings are canonical data, not an extra live configuration channel.
	var actual public.Kudurru
	if err := follower.Get(t.Context(), store.Key{Kind: "Kudurru", Name: "default"}, &actual); err != nil || actual.UID != settings.UID {
		t.Fatal("follower did not install actual canonical object")
	}
}
