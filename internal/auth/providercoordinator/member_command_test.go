package providercoordinator

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/auth/providerresources"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type memberOutputFixture struct {
	*fixture
	connection tls.ConnectionState
	remote     string
}

func memberFinite(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func newMemberOutputFixture(t *testing.T) memberOutputFixture {
	t.Helper()
	f := newFixture(t)
	local, err := memberidentity.Create(filepath.Join(t.TempDir(), "local.json"), f.store.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	_, cut, err := f.store.ReadServingCandidate(memberFinite(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := writer(t, f.store)
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := f.store.Command(ctx, f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.StageEnrollmentPrimary(ctx, cut, local, "primary", "127.0.0.1:8082")
	}); err != nil {
		t.Fatal(err)
	}
	remote, err := memberidentity.Create(filepath.Join(t.TempDir(), "remote.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	change := func(fn func(context.Context, *sqlite.CommandTx, *persistence.Original) error) {
		i, err := f.store.ReadGeneration(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		owner, err := accepted.DecodeRows(i.Payload.Membership, i.Term.ClusterID)
		if err != nil {
			t.Fatal(err)
		}
		ctx, _ := writer(t, f.store)
		ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		original, err := persistence.Capture(ctx, owner, func() error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if err := f.store.Command(ctx, f.store.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error { return fn(ctx, tx, original) }); err != nil {
			t.Fatal(err)
		}
	}
	var invite enrollment.Invitation
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		var err error
		_, invite, err = persistence.Issue(ctx, tx, original, "peer", "127.0.0.2:8082", time.Now().Add(time.Minute))
		return err
	})
	proposal, err := invite.Propose(remote)
	if err != nil {
		t.Fatal(err)
	}
	change(func(ctx context.Context, tx *sqlite.CommandTx, original *persistence.Original) error {
		_, err := persistence.Accept(ctx, tx, original, proposal)
		return err
	})
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
	done := make(chan error, 1)
	go func() { done <- client.HandshakeContext(memberFinite(t)) }()
	serverErr := server.HandshakeContext(memberFinite(t))
	clientErr := <-done
	if serverErr != nil || clientErr != nil {
		t.Fatal("actual enrolled TLS handshake", serverErr, clientErr)
	}
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	return memberOutputFixture{fixture: f, connection: server.ConnectionState(), remote: remote.Trust().MemberID()}
}

func (f memberOutputFixture) request(t *testing.T) (context.Context, serving.TermActivation, *sqlite.MemberRequest) {
	t.Helper()
	ctx, target := writer(t, f.store)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	t.Cleanup(cancel)
	r, err := f.store.CaptureMemberRequest(ctx, target.Term.ClusterID, f.remote, &f.connection)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, target, r
}

func (f memberOutputFixture) authorize(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
	_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: f.wire}, rbac.Attributes{Resource: "stats", Verb: "read"}, time.Now)
	return err
}

func TestMemberCommandOutputRetainsAllPinsThroughEmission(t *testing.T) {
	f := newMemberOutputFixture(t)
	ctx, target, request := f.request(t)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	finish := func() { once.Do(func() { close(release) }) }
	defer finish()
	type outcome struct {
		committed bool
		err       error
	}
	done := make(chan outcome, 1)
	go func() {
		committed, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			if err := f.authorize(ctx, tx, deps); err != nil {
				return err
			}
			return tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-output", Data: []byte(`{"ok":true}`)})
		}, func(output context.Context) error {
			close(entered)
			select {
			case <-release:
				return nil
			case <-output.Done():
				return output.Err()
			}
		})
		done <- outcome{committed, err}
	}()
	select {
	case <-entered:
	case result := <-done:
		t.Fatal("output not entered", result.committed, result.err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, pin := range []func(context.Context) error{
		func(ctx context.Context) error {
			_, release, err := f.publisher.Pin(ctx)
			if release != nil {
				release()
			}
			return err
		},
		func(ctx context.Context) error {
			return f.resources.WithReplacement(ctx, func(*providerresources.Replacement) error { return nil })
		},
	} {
		ctx, cancel := context.WithTimeout(t.Context(), 25*time.Millisecond)
		err := pin(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("replacement crossed emission", err)
		}
	}
	swapped := make(chan struct{})
	go func() {
		f.host.install(&hostGeneration{id: "next", allowed: true, secret: "replacement"})
		close(swapped)
	}()
	select {
	case <-swapped:
		t.Fatal("host replaced during emission")
	case <-time.After(25 * time.Millisecond):
	}
	finish()
	result := <-done
	if !result.committed || result.err != nil {
		t.Fatal("actual completion lost", result)
	}
	select {
	case <-swapped:
	case <-ctx.Done():
		t.Fatal("host pin leaked")
	}
	if err := request.WithCommandOutput(func(context.Context) error { t.Error("second output entered"); return nil }); err == nil {
		t.Fatal("output reused")
	}
}

func TestMemberCommandOutputSeparatesCommitAndTransportFailure(t *testing.T) {
	for _, mode := range []string{"noop", "write", "cancel", "domain-refusal"} {
		t.Run(mode, func(t *testing.T) {
			f := newMemberOutputFixture(t)
			ctx, target, request := f.request(t)
			ctx, cancel := context.WithCancel(ctx)
			defer cancel()
			before, _ := f.store.ReadGeneration(t.Context())
			failed := errors.New("transport failed")
			emitted := false
			committed, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
				if mode == "domain-refusal" {
					_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, deps, servingauth.Credential{Kind: servingauth.APIToken, Value: "forged"}, rbac.Attributes{Resource: "stats", Verb: "read"}, time.Now)
					return err
				}
				if err := f.authorize(ctx, tx, deps); err != nil {
					return err
				}
				if mode == "noop" {
					return nil
				}
				return tx.PutSection(ctx, "dedup", replication.Entry{Key: "member-output", Data: []byte(`{"ok":true}`)})
			}, func(output context.Context) error {
				emitted = true
				if mode == "cancel" {
					cancel()
					<-output.Done()
					return output.Err()
				}
				return failed
			})
			if mode == "domain-refusal" {
				if committed || emitted || err == nil {
					t.Fatal("peer identity replaced raw proof", committed, emitted, err)
				}
			} else if !committed || !emitted || err == nil {
				t.Fatal("transport result replaced completion", committed, emitted, err)
			}
			after, _ := f.store.ReadGeneration(t.Context())
			changed := !before.Generation().Same(after.Generation())
			if changed != (mode == "write" || mode == "cancel") {
				t.Fatal("wrong durable outcome", mode, changed)
			}
		})
	}
}

func TestMemberCommandOutputUsesEarlierHostDeadlineOnActualSocket(t *testing.T) {
	f := newMemberOutputFixture(t)
	deadline := time.Now().Add(2 * time.Second)
	f.host.install(&hostGeneration{id: "bounded-output", allowed: true, secret: "test-only", deadline: deadline})
	wait(t, func() bool { return f.publisher.Status().ProtectedReady })
	ctx, target, request := f.request(t)
	a, b := net.Pipe()
	defer func() { _ = a.Close(); _ = b.Close() }()
	emitted := false
	committed, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, f.authorize, func(output context.Context) error {
		emitted = true
		bound, finite := output.Deadline()
		if !finite || !bound.Equal(deadline) {
			t.Error("original host bound not retained", bound, deadline)
			return ErrInvalid
		}
		if err := a.SetWriteDeadline(bound); err != nil {
			return err
		}
		// Real blocked output must finish at the earlier bound. The receiver
		// deliberately never reads; no timer pretends that bytes were flushed.
		_, err := a.Write([]byte("bounded response"))
		return err
	})
	var socketError net.Error
	if !committed || !emitted || !errors.As(err, &socketError) || !socketError.Timeout() {
		t.Fatal("socket timeout changed completed no-op", committed, emitted, err)
	}
}

func TestMemberCommandOutputRefusesMissingOriginalWriterAndLifetime(t *testing.T) {
	f := newMemberOutputFixture(t)
	for _, mode := range []string{"missing-writer", "unbounded", "too-long", "canceled", "wrong-target", "nil-request"} {
		t.Run(mode, func(t *testing.T) {
			ctx, target, request := f.request(t)
			switch mode {
			case "missing-writer":
				ctx = memberFinite(t)
			case "unbounded":
				ctx, _ = writer(t, f.store)
			case "too-long":
				ctx, _ = writer(t, f.store)
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, time.Minute)
				defer cancel()
			case "canceled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "wrong-target":
				target.Incarnation++
			case "nil-request":
				request = nil
			}
			called := false
			committed, err := f.coordinator.WithMemberCommandOutput(ctx, target, request, func(context.Context, *sqlite.CommandTx, servingauth.Dependencies) error { called = true; return nil }, func(context.Context) error { called = true; return nil })
			if committed || called || err == nil {
				t.Fatal("invalid original admission entered", committed, called, err)
			}
		})
	}
	if _, err := new(providerresources.Pinned).Deadline(); err == nil {
		t.Fatal("zero pin supplied a lifetime")
	}
}
