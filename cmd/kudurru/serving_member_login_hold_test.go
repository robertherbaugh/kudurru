package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth/users"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// installedRevision is the revision of the image that a node has installed or,
// on the primary, committed (a mirror's installed digest differs from none).
func installedRevision(s *sqlite.Store) uint64 { return s.Authority().Installed.Revision }

// A sign-in through an enrolled follower is bound to the source its request
// captured, like every other forwarded command, so it holds the follower's
// scheduled pull while it runs (D375, D424). Without the hold a pull that
// installs a commit of the primary between the request's capture and its
// forward cut the sign-in, which answered 503 unavailable "the enrolled
// primary is unavailable; the login was not attempted" for a login that the
// primary was never asked to run (the enrolled-cluster e2e flake in
// TestEnrolledManagedAnchorsOfflineRecovery: `kudurru login --username`
// against the follower, which hides the answer's text).
func TestRootMemberLoginHoldsScheduledPull(t *testing.T) {
	const password = "follower-login-hold-password-1" // #nosec G101 -- disposable fixture credential
	f := newMemberForwardHTTPFixture(t, false, func(f *memberForwardHTTPFixture) {
		f.scheduledPull, f.pullInterval = true, 20*time.Millisecond
	})
	primary, follower := f.primary.s.opts.Store, f.follower.opts.Store
	hash, err := users.NewHasher(1).Hash(t.Context(), password)
	if err != nil {
		t.Fatal(err)
	}
	primaryCommand := func(fn func(context.Context, *sqlite.CommandTx) error) {
		t.Helper()
		writer, err := primary.WriterToken()
		if err != nil {
			t.Fatal(err)
		}
		if err := primary.Command(sqlite.WithWriterToken(t.Context(), writer), writer.Term(), fn); err != nil {
			t.Fatal(err)
		}
	}
	primaryCommand(func(ctx context.Context, tx *sqlite.CommandTx) error {
		var user internalv1.LocalUser
		if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: "root-command"}, &user); err != nil {
			return err
		}
		user.Spec.PasswordHash = hash
		_, err := tx.Apply(ctx, &user, store.ApplyOptions{Manager: store.ManagerSystem})
		return err
	})
	f.primary.s.publisher.Wake()
	// The follower has installed everything the primary has, so only the
	// commit below is left for a pull to install.
	if !kudurrutestEventually(20*time.Second, func() bool {
		return installedRevision(follower) == installedRevision(primary) &&
			f.follower.publisher.Status().ProtectedReady && f.follower.publisher.Status().Served.Base.Same(follower.Authority().Installed)
	}) {
		t.Fatalf("the follower did not install the primary's state: %+v, primary %+v", follower.Authority().Installed, primary.Authority().Installed)
	}
	// The request captures its source and stops at its first body read.
	held := &memberForwardBodyHold{entered: make(chan struct{}), release: make(chan struct{})}
	f.hold.Store(held)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	type answer struct {
		status int
		body   []byte
		err    error
	}
	finished := make(chan answer, 1)
	go func() {
		encoded, err := json.Marshal(api.LoginRequest{Username: "root-command", Password: password})
		if err != nil {
			finished <- answer{err: err}
			return
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, f.live.URL+api.BasePath+"/auth/login", bytes.NewReader(encoded))
		if err != nil {
			finished <- answer{err: err}
			return
		}
		request.Header.Set("Content-Type", "application/json")
		response, err := f.live.Client().Do(request)
		if err != nil {
			finished <- answer{err: err}
			return
		}
		defer func() { _ = response.Body.Close() }()
		body, err := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		finished <- answer{response.StatusCode, body, err}
	}()
	select {
	case <-held.entered:
	case got := <-finished:
		t.Fatalf("the login answered before its body was read: status=%d err=%v body=%s", got.status, got.err, got.body)
	case <-ctx.Done():
		t.Fatal("the login did not reach its body read")
	}
	// The primary commits something that a pull would install. Hundreds of
	// pull intervals later the held request must still have its source.
	before := follower.Authority().InstallSequence
	primaryCommand(func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, "dedup", replication.Entry{Key: "login-hold-interposition", Data: []byte(`{"changed":true}`)})
	})
	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if follower.Authority().InstallSequence != before {
			t.Error("the scheduled pull installed the primary's commit while a sign-in through the follower ran")
			break
		}
	}
	close(held.release)
	select {
	case got := <-finished:
		var info api.SessionInfo
		if got.err != nil || got.status != http.StatusOK || json.Unmarshal(got.body, &info) != nil || info.CSRF == "" {
			t.Fatalf("sign-in through the follower while the primary committed: status=%d err=%v body=%s", got.status, got.err, got.body)
		}
	case <-ctx.Done():
		t.Fatal("the held login did not join")
	}
	// The hold only delays the pull: it installs the commit once the sign-in
	// has ended.
	if !kudurrutestEventually(20*time.Second, func() bool {
		return installedRevision(follower) == installedRevision(primary)
	}) {
		t.Fatalf("the follower did not install the primary's commit after the sign-in: %+v, primary %+v", follower.Authority().Installed, primary.Authority().Installed)
	}
}
