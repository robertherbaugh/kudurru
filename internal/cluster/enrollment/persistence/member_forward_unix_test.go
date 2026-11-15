//go:build unix

package persistence_test

import (
	"context"
	"errors"
	"net/http"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/cluster/replica"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type forwardFixture struct {
	acquisitionFixture
	forward *replica.EnrolledForwarder
	domain  *memberServerDomain
	posts   *atomic.Int32
}

func newForwardFixture(t *testing.T, after func() error) forwardFixture {
	t.Helper()
	var commands atomic.Value
	posts := new(atomic.Int32)
	f := newAcquisitionFixture(t, func(read http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == replica.PathWrite {
				posts.Add(1)
				commands.Load().(http.Handler).ServeHTTP(w, r)
				return
			}
			read.ServeHTTP(w, r)
		})
	})
	domain := new(memberServerDomain)
	server, credentials := memberServer(t, f.memberFixture, memberCommandHandler(f.memberFixture, domain, after))
	commands.Store(server.Handler())
	forward, err := replica.NewEnrolledForwarder(replica.EnrolledForwardOptions{Store: f.mirror, Identity: f.remote, Credentials: credentials})
	if err != nil {
		t.Fatal(err)
	}
	return forwardFixture{f, forward, domain, posts}
}

func (f forwardFixture) capture(ctx context.Context, t *testing.T) (*sqlite.MemberForwardRequest, replica.Command) {
	t.Helper()
	_, token, err := f.mirror.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	request, err := f.mirror.CaptureMemberForward(ctx, token, f.remote.Trust())
	if err != nil {
		t.Fatal(err)
	}
	command := memberCommand(f.memberFixture)
	command.NotAfter = request.Deadline()
	return request, command
}

func TestMemberForwardActualCommandAndSingleScopedResponse(t *testing.T) {
	f := newForwardFixture(t, nil)
	ctx := finite(t)
	request, command := f.capture(ctx, t)
	foreign, _ := f.capture(ctx, t)
	before := f.mirror.Authority()
	var escaped context.Context
	emissions := 0
	received, err := f.forward.Forward(ctx, request, command, func(scope context.Context, out replica.Outcome) error {
		emissions++
		if out.Status != http.StatusOK || string(out.Body) != `{"changed":true}` {
			t.Fatal("wrong actual primary result")
		}
		escaped = scope
		if err := foreign.Check(scope); err == nil {
			t.Fatal("foreign capture authenticated output")
		}
		for _, bad := range []context.Context{nil, ctx, context.WithoutCancel(scope)} {
			if err := request.Check(bad); err == nil {
				t.Fatal("unissued or unbounded context authenticated output")
			}
		}
		extended, cancel := context.WithDeadline(context.WithoutCancel(scope), request.Deadline().Add(time.Second))
		defer cancel()
		if err := request.Check(extended); err == nil {
			t.Fatal("extended deadline authenticated output")
		}
		copied := *request
		return copied.Check(scope)
	})
	if err != nil || !received || emissions != 1 || f.posts.Load() != 1 || f.domain.executions.Load() != 1 {
		t.Fatal("one actual command and response not preserved", received, emissions, f.posts.Load(), f.domain.executions.Load(), err)
	}
	if err := request.Check(escaped); err == nil {
		t.Fatal("response scope escaped original callback")
	}
	if !before.Installed.Same(f.mirror.Authority().Installed) || before.InstallSequence != f.mirror.Authority().InstallSequence {
		t.Fatal("forwarding installed or published the primary result")
	}
	copied := *request
	if got, err := f.forward.Forward(ctx, &copied, command, func(context.Context, replica.Outcome) error { t.Fatal("copied attempt emitted twice"); return nil }); err == nil || got || f.posts.Load() != 1 {
		t.Fatal("copied attempt replayed a command", got, err)
	}
}

func TestMemberForwardRejectsRetiredSourceAndForeignIdentityBeforeNetwork(t *testing.T) {
	for _, mode := range []string{"new-generation", "higher-term", "canceled", "wrong-store", "replacement-identity", "different-term", "extended-command"} {
		t.Run(mode, func(t *testing.T) {
			f := newForwardFixture(t, nil)
			ctx, cancel := context.WithCancel(finite(t))
			defer cancel()
			request, command := f.capture(ctx, t)
			forward := f.forward
			switch mode {
			case "new-generation":
				if err := f.s.Command(carried(t, f.s), f.s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					return tx.PutSection(ctx, "dedup", replication.Entry{Key: "interposed-generation", Data: []byte(`{"changed":true}`)})
				}); err != nil {
					t.Fatal(err)
				}
				installAcquisitionImage(t, f.mirror, image(t, f.s))
			case "higher-term":
				next := request.OriginalTerm()
				next.Epoch++
				next.PromotionID = "interposed-term"
				if _, err := f.mirror.ObserveAuthority(finite(t), next); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel() // A fresh invocation context must not detach this cancellation.
			case "wrong-store", "replacement-identity":
				_, credentials := memberServer(t, f.memberFixture, nil)
				local, receiver := f.remote, f.mirror
				if mode == "wrong-store" {
					receiver = f.s
				} else {
					var err error
					local, err = memberidentity.Create(filepath.Join(t.TempDir(), "replacement.json"), f.remote.Trust().MemberID())
					if err != nil {
						t.Fatal(err)
					}
				}
				var err error
				forward, err = replica.NewEnrolledForwarder(replica.EnrolledForwardOptions{Store: receiver, Identity: local, Credentials: credentials})
				if err != nil {
					t.Fatal(err)
				}
			case "different-term":
				command.ExpectedTerm.Epoch++
			case "extended-command":
				command.NotAfter = request.Deadline().Add(time.Second)
			}
			called := false
			received, err := forward.Forward(finite(t), request, command, func(context.Context, replica.Outcome) error { called = true; return nil })
			if err == nil || received || called || f.posts.Load() != 0 || f.domain.executions.Load() != 0 {
				t.Fatal("retired/foreign request reached network or output", received, called, f.posts.Load(), err)
			}
		})
	}
}

func TestMemberForwardOriginalCaptureRefusesInvalidCutsAndBounds(t *testing.T) {
	f := newForwardFixture(t, nil)
	_, token, err := f.mirror.ReadServingCandidate(finite(t))
	if err != nil {
		t.Fatal(err)
	}
	_, primary, err := f.s.ReadServingCandidate(finite(t))
	if err != nil {
		t.Fatal(err)
	}
	long, cancelLong := context.WithTimeout(t.Context(), time.Minute)
	defer cancelLong()
	canceled, cancel := context.WithCancel(finite(t))
	cancel()
	for _, ctx := range []context.Context{nil, t.Context(), long, canceled, carried(t, f.s)} {
		if _, err := f.mirror.CaptureMemberForward(ctx, token, f.remote.Trust()); err == nil {
			t.Fatal("invalid lifetime/writer accepted")
		}
	}
	for _, invalid := range []sqlite.GenerationToken{{}, primary} {
		if _, err := f.mirror.CaptureMemberForward(finite(t), invalid, f.remote.Trust()); err == nil {
			t.Fatal("foreign or empty token accepted")
		}
	}
	if _, err := f.s.CaptureMemberForward(finite(t), primary, f.local.Trust()); err == nil {
		t.Fatal("primary admitted outgoing forwarding")
	}
}

func TestMemberForwardExcludesReplacementThroughOutputAndPreservesReceived(t *testing.T) {
	f := newForwardFixture(t, nil)
	ctx, cancel := context.WithCancel(finite(t))
	defer cancel()
	request, command := f.capture(ctx, t)
	entered, release := make(chan struct{}), make(chan struct{})
	defer close(release)
	type result struct {
		received bool
		err      error
	}
	finished := make(chan result, 1)
	go func() {
		received, err := f.forward.Forward(ctx, request, command, func(scope context.Context, _ replica.Outcome) error {
			close(entered)
			select {
			case <-scope.Done():
				return request.Check(scope)
			case <-release:
				return errors.New("test ended")
			}
		})
		finished <- result{received, err}
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("actual response did not enter output")
	}
	queued, cancelQueued := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancelQueued()
	next := request.OriginalTerm()
	next.Epoch++
	next.PromotionID = "held-output-observation"
	// ObserveAuthority uses an unconditional writer lock, so join it after
	// releasing output. A separate acquisition proves bounded queue refusal.
	changed := make(chan error, 1)
	changeContext := finite(t)
	go func() { _, err := f.mirror.ObserveAuthority(changeContext, next); changed <- err }()
	if err := f.mirror.WithMemberAcquisition(queued, f.remote.Trust(), func(context.Context, *sqlite.MemberAcquisition) error {
		t.Error("queued source entered held output")
		return nil
	}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("queue lost original deadline", err)
	}
	select {
	case err := <-changed:
		t.Fatal("replacement entered during response output", err)
	default:
	}
	cancel()
	select {
	case got := <-finished:
		if !got.received || !errors.Is(got.err, context.Canceled) {
			t.Fatal("response receipt lost after canceled output", got)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled output did not join")
	}
	select {
	case err := <-changed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement remained blocked after output joined")
	}
	if f.posts.Load() != 1 || f.domain.executions.Load() != 1 {
		t.Fatal("canceled output retried command")
	}
}

func TestMemberForwardLostPrimaryOutputDoesNotReplayCommittedCommand(t *testing.T) {
	f := newForwardFixture(t, func() error { return errors.New("actual primary output failed after commit") })
	ctx := finite(t)
	request, command := f.capture(ctx, t)
	called := false
	received, err := f.forward.Forward(ctx, request, command, func(context.Context, replica.Outcome) error { called = true; return nil })
	if err == nil || received || called || f.domain.executions.Load() != 1 || f.posts.Load() != 1 {
		t.Fatal("lost postcommit output became received/replayed success", received, called, err)
	}
	if _, err := f.forward.Forward(ctx, request, command, func(context.Context, replica.Outcome) error { called = true; return nil }); err == nil || called || f.posts.Load() != 1 {
		t.Fatal("failed attempt renewed its command", err)
	}
	effects := 0
	for _, row := range image(t, f.s).Payload.Dedup {
		if row.Key == "member-server-effect" {
			effects++
		}
	}
	if effects != 1 {
		t.Fatal("actual committed effect lost after failed primary output", effects)
	}
}
