package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// The sign-in's pull hold (D424) keeps the captured source valid until the
// forward. It ends once the primary has committed the session, so that when
// the explicit pull after the commit fails (a short loss of the primary right
// after it) the scheduled pull installs the session and the sign-in is still
// answered with its cookie, in one attempt, instead of waiting for a pull that
// its own hold blocks until the sign-in's deadline.
func TestRootMemberLoginReleasesPullHoldAfterCommit(t *testing.T) {
	const password = "follower-login-release-password-1" // #nosec G101 -- disposable fixture credential
	f := newMemberForwardHTTPFixture(t, false, func(f *memberForwardHTTPFixture) {
		f.scheduledPull, f.pullInterval = true, 20*time.Millisecond
		f.configureFollower = func(follower *servingRuntime) {
			follower.memberSelfPull = func(context.Context) error { return errors.New("the primary was unreachable for the explicit pull") }
		}
	})
	primary, follower := f.primary.s.opts.Store, f.follower.opts.Store
	hash, err := users.NewHasher(1).Hash(t.Context(), password)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := primary.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := primary.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	f.primary.s.publisher.Wake()
	if !kudurrutestEventually(20*time.Second, func() bool {
		return installedRevision(follower) == installedRevision(primary) &&
			f.follower.publisher.Status().ProtectedReady && f.follower.publisher.Status().Served.Base.Same(follower.Authority().Installed)
	}) {
		t.Fatalf("the follower did not install the primary's state: %+v, primary %+v", follower.Authority().Installed, primary.Authority().Installed)
	}
	// #nosec G117 -- disposable fixture password of a test-only server
	answer := f.request(t, nil, http.MethodPost, "/auth/login", "", api.LoginRequest{Username: "root-command", Password: password})
	var info api.SessionInfo
	if answer.status != http.StatusOK || json.Unmarshal(answer.raw, &info) != nil || info.CSRF == "" {
		t.Fatalf("sign-in through the follower whose explicit pull fails: %d %s", answer.status, answer.raw)
	}
}
