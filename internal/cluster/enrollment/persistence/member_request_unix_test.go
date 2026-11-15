//go:build unix

package persistence_test

import (
	"context"
	"crypto/tls"
	"database/sql"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type memberFixture struct {
	s             *sqlite.Store
	path          string
	local, remote *memberidentity.Identity
	tls           tls.ConnectionState
}

// Use an actual mutual TLS handshake, not caller-constructed VerifiedChains.
// Storage cases use this retained transport evidence under their own original
// finite operation; live HTTP Write+Flush is a separate mounted test.
func peerConnection(t *testing.T, local, remote *memberidentity.Identity) tls.ConnectionState {
	t.Helper()
	serverConfig, err := local.ServerTLS([]memberidentity.Trust{remote.Trust()})
	if err != nil {
		t.Fatal(err)
	}
	clientConfig, err := remote.ClientTLS(local.Trust())
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	server, client := tls.Server(a, serverConfig), tls.Client(b, clientConfig)
	ctx := finite(t)
	done := make(chan error, 1)
	go func() { done <- client.HandshakeContext(ctx) }()
	err = server.HandshakeContext(ctx)
	clientErr := <-done
	if err != nil || clientErr != nil {
		t.Fatal("actual member handshake failed", err, clientErr)
	}
	return server.ConnectionState()
}

func newMemberFixture(t *testing.T) memberFixture {
	t.Helper()
	s, path := initialized(t)
	local := primaryIdentity(t, s)
	cut := primaryCut(t, s)
	if err := s.Command(carried(t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", "127.0.0.1:8082")
	}); err != nil {
		t.Fatal(err)
	}
	remote, err := memberidentity.Create(filepath.Join(t.TempDir(), "peer.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	ctx, o := original(t, s, func() error { return nil })
	var invite enrollment.Invitation
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		_, invite, err = persistence.Issue(ctx, tx, o, "follower", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	proposal, err := invite.Propose(remote)
	if err != nil || accept(t, s, proposal) != nil {
		t.Fatal("could not enroll actual peer", err)
	}
	return memberFixture{s: s, path: path, local: local, remote: remote, tls: peerConnection(t, local, remote)}
}

func (f memberFixture) capture(ctx context.Context, t *testing.T) *sqlite.MemberRequest {
	t.Helper()
	r, err := f.s.CaptureMemberRequest(ctx, f.s.Authority().Term.ClusterID, f.remote.Trust().MemberID(), &f.tls)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (f memberFixture) revoke(t *testing.T) error {
	t.Helper()
	ctx, original := original(t, f.s, func() error { return nil })
	return f.s.Command(ctx, f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Revoke(ctx, tx, original, f.remote.Trust().MemberID())
		return err
	})
}

func TestMemberRequestActualImageAndOwnCommandCompletion(t *testing.T) {
	for _, mode := range []string{"image", "command", "carried-command", "noop", "local-audit"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			ctx := finite(t)
			if mode == "carried-command" {
				ctx = carried(t, f.s)
			}
			r := f.capture(ctx, t)
			copyRequest := *r
			before := image(t, f.s)
			if mode == "image" {
				got, err := r.ReadImage()
				if err != nil || !got.Generation().Same(before.Generation()) {
					t.Fatal("actual complete image refused", err)
				}
			} else {
				if err := f.s.MemberCommand(ctx, r, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if mode == "command" || mode == "carried-command" {
						return tx.PutSection(ctx, "dedup", replication.Entry{Key: "actual-member-command", Data: []byte(`{"ok":true}`)})
					}
					if mode == "local-audit" {
						return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "member.command"})
					}
					return nil
				}); err != nil {
					t.Fatal("original peer command failed", err)
				}
			}
			emissions := 0
			if err := r.WithOutput(func(original context.Context) error {
				emissions++
				deadline, finite := original.Deadline()
				bound, _ := ctx.Deadline()
				if !finite || deadline.After(bound) {
					return errors.New("output renewed original bound")
				}
				return nil
			}); err != nil || emissions != 1 {
				t.Fatal("actual own completion could not emit", err)
			}
			if err := copyRequest.WithOutput(func(context.Context) error { emissions++; return nil }); err == nil || emissions != 1 {
				t.Fatal("copied handle replayed output", err)
			}
			called := false
			if err := f.s.MemberCommand(ctx, &copyRequest, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); err == nil || called {
				t.Fatal("copied handle repeated original command", err)
			}
			if got, err := copyRequest.ReadImage(); err == nil || got != nil {
				t.Fatal("copied handle switched to image acquisition", err)
			}
			wantRevision := before.Revision
			if mode == "command" || mode == "carried-command" {
				wantRevision++
			}
			if image(t, f.s).Revision != wantRevision {
				t.Fatal("peer receipt changed source revision semantics")
			}
		})
	}
}

func TestMemberRequestRevocationAndSourceInterpositionRefuseOriginal(t *testing.T) {
	for _, mode := range []string{"before-image", "before-command", "after-image", "after-command", "unrelated-image-change", "higher-authority", "cancel-after-command", "foreign-store", "foreign-carried-writer", "zero-carried-writer"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			ctx, cancel := context.WithCancel(finite(t))
			defer cancel()
			r := f.capture(ctx, t)
			called := false
			if mode == "foreign-store" || mode == "foreign-carried-writer" || mode == "zero-carried-writer" {
				other, _ := initialized(t)
				target, commandCtx := other, ctx
				if mode == "foreign-carried-writer" {
					token, err := other.WriterToken()
					if err != nil {
						t.Fatal(err)
					}
					target, commandCtx = f.s, sqlite.WithWriterToken(ctx, token)
				}
				if mode == "zero-carried-writer" {
					target, commandCtx = f.s, sqlite.WithWriterToken(ctx, sqlite.WriterToken{})
				}
				if err := target.MemberCommand(commandCtx, r, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); err == nil || called {
					t.Fatal("foreign Store or writer adopted original peer", err)
				}
				return
			}
			if mode == "before-image" || mode == "before-command" {
				if err := f.revoke(t); err != nil {
					t.Fatal(err)
				}
				if mode == "before-image" {
					if got, err := r.ReadImage(); err == nil || got != nil {
						t.Fatal("revoked peer acquired bytes")
					}
				} else if err := f.s.MemberCommand(ctx, r, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); err == nil || called {
					t.Fatal("revoked peer entered command", err)
				}
				return
			}
			if mode == "after-command" || mode == "cancel-after-command" {
				if err := f.s.MemberCommand(ctx, r, func(context.Context, *sqlite.CommandTx) error { return nil }); err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.ReadImage(); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "higher-authority":
				next := f.s.Authority().Term
				next.Epoch++
				next.PrimaryID, next.PromotionID = uuid.NewString(), uuid.NewString()
				if _, err := f.s.ObserveAuthority(ctx, next); err != nil {
					t.Fatal(err)
				}
			case "cancel-after-command":
				cancel()
			case "unrelated-image-change":
				if err := f.s.Command(carried(t, f.s), f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					return tx.PutSection(ctx, "runtime", replication.Entry{Key: "other", Data: []byte(`{}`)})
				}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := f.revoke(t); err != nil {
					t.Fatal(err)
				}
			}
			if err := r.WithOutput(func(context.Context) error { called = true; return nil }); err == nil || called {
				t.Fatal("changed original emitted old data", err)
			}
		})
	}
}

func TestMemberCommandFinalRefusalAndOwnerABARollBack(t *testing.T) {
	for _, mode := range []string{"authorization", "original-cancel", "final-original-cancel", "final-noop-cancel", "owner-aba", "panic", "unknown-sql787"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			originalCtx, cancel := context.WithCancel(finite(t))
			defer cancel()
			r := f.capture(originalCtx, t)
			before := image(t, f.s)
			refusal := errors.New("domain proof expired")
			if mode == "unknown-sql787" {
				db, err := sql.Open("sqlite", f.path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				for _, q := range []string{
					`CREATE TABLE member_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TABLE member_child(parent INTEGER REFERENCES member_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TRIGGER member_unknown AFTER INSERT ON replication_sections WHEN NEW.key='member-result' BEGIN INSERT INTO member_child(parent) VALUES (1); END`,
				} {
					if _, err := db.ExecContext(t.Context(), q); err != nil {
						t.Fatal(err)
					}
				}
			}
			var err error
			panicked := false
			func() {
				defer func() {
					if p := recover(); p != nil {
						if p != refusal {
							panic(p)
						}
						panicked = true
					}
				}()
				err = f.s.MemberCommand(finite(t), r, func(ctx context.Context, tx *sqlite.CommandTx) error {
					if mode == "final-noop-cancel" {
						return tx.RequireCommitCheck(func(context.Context) error { cancel(); return nil })
					}
					if err := tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-result", Data: []byte(`{}`)}); err != nil {
						return err
					}
					switch mode {
					case "authorization":
						return tx.RequireCommitCheck(func(context.Context) error { return refusal })
					case "original-cancel":
						cancel() // The separately supplied command context remains alive.
					case "final-original-cancel":
						return tx.RequireCommitCheck(func(context.Context) error { cancel(); return nil })
					case "owner-aba":
						owner, err := persistence.Load(ctx, tx)
						if err != nil {
							return err
						}
						rows, err := owner.Rows()
						if err != nil {
							return err
						}
						if err := tx.DeleteSection(ctx, accepted.Section, accepted.RootKey); err != nil {
							return err
						}
						return tx.PutSection(ctx, accepted.Section, rows[0])
					case "panic":
						panic(refusal)
					}
					return nil
				})
			}()
			switch mode {
			case "panic":
				if !panicked {
					t.Fatal("callback panic was swallowed")
				}
			case "authorization":
				if !errors.Is(err, refusal) {
					t.Fatal(err)
				}
			case "original-cancel", "final-original-cancel", "final-noop-cancel":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "owner-aba":
				if !errors.Is(err, replication.ErrCAS) {
					t.Fatal(err)
				}
			case "unknown-sql787":
				var code interface{ Code() int }
				if !errors.As(err, &code) || code.Code() != 787 || !errors.Is(err, sqlite.ErrMemberCompletionUnknown) {
					t.Fatal("fixture missed actual uncertain commit", err)
				}
			}
			if !image(t, f.s).Generation().Same(before.Generation()) {
				t.Fatal("failed peer command changed canonical image")
			}
			called := false
			if err := r.WithOutput(func(context.Context) error { called = true; return nil }); err == nil || called {
				t.Fatal("refused/unknown command issued an output receipt", err)
			}
		})
	}
}

func TestMemberRequestOutputExcludesRevocationUntilActualReturn(t *testing.T) {
	f := newMemberFixture(t)
	ctx, cancel := context.WithCancel(finite(t))
	defer cancel()
	r := f.capture(ctx, t)
	if _, err := r.ReadImage(); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	output := make(chan error, 1)
	go func() {
		output <- r.WithOutput(func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	// Capture independent revocation through a read transaction while output
	// holds wmu. Its actual command must wait for output to return.
	revokeCtx, revokeOriginal := original(t, f.s, func() error { return nil })
	started, revoked := make(chan struct{}), make(chan error, 1)
	go func() {
		close(started)
		revoked <- f.s.Command(revokeCtx, f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := persistence.Revoke(ctx, tx, revokeOriginal, f.remote.Trust().MemberID())
			return err
		})
	}()
	<-started
	cancel()
	select {
	case err := <-revoked:
		close(release)
		t.Fatal("cancellation released exclusion before actual output returned", err)
	case <-time.After(25 * time.Millisecond):
	}
	close(release)
	if err := <-output; !errors.Is(err, context.Canceled) {
		t.Fatal("canceled emission reported success", err)
	}
	if err := <-revoked; err != nil {
		t.Fatal(err)
	}
	members := owner(t, f.s).State().Members()
	for _, m := range members {
		if m.ID == f.remote.Trust().MemberID() && !m.Revoked {
			t.Fatal("joined revocation did not commit")
		}
	}
}

func TestMemberRequestCaptureRequiresExactEnrolledTLSAndOriginalContext(t *testing.T) {
	f := newMemberFixture(t)
	before := image(t, f.s)
	for _, mode := range []string{"nil", "unbounded", "excessive", "canceled", "self", "unknown", "wrong-cluster", "wrong-tls", "zero-tls", "foreign-carried-writer", "zero-carried-writer"} {
		t.Run(mode, func(t *testing.T) {
			ctx := finite(t)
			member, cluster := f.remote.Trust().MemberID(), f.s.Authority().Term.ClusterID
			connection := &f.tls
			switch mode {
			case "nil":
				ctx = nil
			case "unbounded":
				ctx = context.Background()
			case "excessive":
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(t.Context(), time.Minute)
				defer cancel()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "self":
				member = f.local.Trust().MemberID()
			case "unknown":
				member = uuid.NewString()
			case "wrong-cluster":
				cluster = uuid.NewString()
			case "zero-carried-writer":
				ctx = sqlite.WithWriterToken(ctx, sqlite.WriterToken{})
			case "foreign-carried-writer":
				other, _ := initialized(t)
				token, err := other.WriterToken()
				if err != nil {
					t.Fatal(err)
				}
				ctx = sqlite.WithWriterToken(ctx, token)
			case "wrong-tls":
				outsider, err := memberidentity.Create(filepath.Join(t.TempDir(), "outsider.json"), f.remote.Trust().MemberID())
				if err != nil {
					t.Fatal(err)
				}
				other := peerConnection(t, f.local, outsider)
				connection = &other
			case "zero-tls":
				connection = &tls.ConnectionState{}
			}
			if request, err := f.s.CaptureMemberRequest(ctx, cluster, member, connection); err == nil || request != nil {
				t.Fatal("invalid original obtained peer request", err)
			}
		})
	}
	if !reflect.DeepEqual(before.Payload, image(t, f.s).Payload) {
		t.Fatal("read-only peer admission mutated enrollment")
	}
}

func TestMemberRequestCanceledWaiterCannotOutwaitOriginal(t *testing.T) {
	for _, mode := range []string{"image", "command", "output"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberFixture(t)
			held := f.capture(finite(t), t)
			if _, err := held.ReadImage(); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(finite(t))
			defer cancel()
			waiter := f.capture(ctx, t)
			if mode == "output" {
				if _, err := waiter.ReadImage(); err != nil {
					t.Fatal(err)
				}
			}
			entered, release := make(chan struct{}), make(chan struct{})
			heldDone := make(chan error, 1)
			go func() {
				heldDone <- held.WithOutput(func(context.Context) error { close(entered); <-release; return nil })
			}()
			<-entered
			defer func() {
				close(release)
				if err := <-heldDone; err != nil {
					t.Error("held output failed", err)
				}
			}()
			started, result := make(chan struct{}), make(chan error, 1)
			go func() {
				close(started)
				switch mode {
				case "image":
					_, err := waiter.ReadImage()
					result <- err
				case "command":
					result <- f.s.MemberCommand(context.Background(), waiter, func(context.Context, *sqlite.CommandTx) error {
						return errors.New("expired waiter reached command")
					})
				case "output":
					result <- waiter.WithOutput(func(context.Context) error { return errors.New("expired waiter emitted") })
				}
			}()
			<-started
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("waiter did not retain original cancellation", err)
				}
			case <-time.After(time.Second):
				t.Fatal("canceled waiter remained blocked behind unrelated output")
			}
		})
	}
}
