package main

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

func anchorFollowerPollerFixture(t *testing.T, endpoint string, seeds []string) (*anchorFollowerFixture, context.Context, *anchorFollowerPoller) {
	t.Helper()
	primary, original, target, host := anchorOperationFixture(t, endpoint, seeds)
	primaryClock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	op, err := primary.captureAnchorOperation(original, target, host, primaryClock, 0)
	if err != nil {
		t.Fatal(err)
	}
	if receipt, err := op.run(); err != nil || receipt.Update.Outcome() != sqlite.AnchorCommitted {
		t.Fatal("original primary bootstrap", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	t.Cleanup(cancel)
	f := newAnchorFollowerFixture(ctx, t, primary)
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	return f, ctx, &anchorFollowerPoller{runtime: f.s, clock: clock}
}

func TestAnchorFollowerPollerNormalCooldownAndIndependentDenial(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 3, nil)
	f, ctx, u := anchorFollowerPollerFixture(t, endpoint, seeds)
	before, authority := anchorFollowerImage(ctx, t, f.store), f.store.Authority()
	started := time.Now()
	first, err := u.step(ctx)
	if err != nil || !first.attempted || first.retired || first.result.branch != sqlite.AnchorPollNormal || first.acquisitionError != nil || first.next.Before(started.Add(trustanchors.MinRefresh)) || first.next.After(time.Now().Add(trustanchors.MinRefresh)) || reads.Load() != 2 {
		t.Fatal("normal follower observation did not schedule a new independent poll", first, err, reads.Load())
	}
	second, err := u.step(ctx)
	if err != nil || second.attempted || !second.next.Equal(first.next) || reads.Load() != 2 || u.offset != 1 {
		t.Fatal("normal observation was immediately reacquired", second, err)
	}
	if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(authority, f.store.Authority()) {
		t.Fatal("normal polling changed canonical data or authority")
	}
	// A foreign cooldown must not suppress this actual source. This changes
	// only local scheduling identity; it invents no Sample or accepted state.
	u.nextFor.settingsVersion = "foreign"
	third, err := u.step(ctx)
	if err != nil || !third.attempted || !third.retired || third.result.branch != sqlite.AnchorPollRevoke || third.result.completion.Outcome() != sqlite.AnchorCommitted || reads.Load() != 3 {
		t.Fatal("independent follower revoke did not durably deny and stop", third, err)
	}
	if _, err := u.step(ctx); !errors.Is(err, sqlite.ErrAnchorUnavailable) || reads.Load() != 3 {
		t.Fatal("retired follower polled again", err)
	}
	if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) || !reflect.DeepEqual(authority, f.store.Authority()) {
		t.Fatal("deny-only polling changed canonical state or authority")
	}
	f.reopen(ctx, t)
	_, token, err := f.store.ReadServingCandidate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.CaptureAnchorPublication(ctx, token); !errors.Is(err, sqlite.ErrAnchorUnavailable) {
		t.Fatal("actual follower reopen lost poller's durable denial", err)
	}
}

// Two actual TCP requests: primary bootstrap, then the follower's independent
// receipt. The callback may alter only that second response. Cleanup joins the
// finite server; it cannot leave an accepted connection behind after a failure.
func anchorFollowerPollerServer(t *testing.T, second func(net.Conn, []byte) []byte) (string, []string, *atomic.Int32) {
	t.Helper()
	seed, signed := anchorWireSignedReply(t)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var reads atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for n := 1; n <= 2; n++ {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			func() {
				defer func() { _ = conn.Close() }()
				if err := conn.SetDeadline(time.Now().Add(4 * time.Second)); err != nil {
					t.Error(err)
					return
				}
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					t.Error(err)
					return
				}
				raw := make([]byte, binary.BigEndian.Uint16(size[:]))
				if _, err := io.ReadFull(conn, raw); err != nil {
					t.Error(err)
					return
				}
				reads.Add(1)
				response := signed(raw)
				if n == 2 {
					response = second(conn, response)
				}
				if response == nil {
					return
				}
				if len(response) > anchorWireLimit {
					t.Error("follower response exceeds one frame")
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(response))) // #nosec G115 -- response length checked against the 65535-byte wire limit above
				_, _ = conn.Write(append(size[:], response...))
			}()
		}
	}()
	t.Cleanup(func() { _ = listener.Close(); <-done })
	return listener.Addr().String(), seed.DS, &reads
}

func TestAnchorFollowerPollerRetriesOnlyIncompleteAcquisition(t *testing.T) {
	for _, kind := range []string{"incomplete", "correlation", "signature", "malformed"} {
		t.Run(kind, func(t *testing.T) {
			endpoint, seeds, reads := anchorFollowerPollerServer(t, func(_ net.Conn, raw []byte) []byte {
				switch kind {
				case "incomplete":
					return nil
				case "correlation":
					raw[0] ^= 1
				case "signature":
					raw[len(raw)-1] ^= 1
				case "malformed":
					return raw[:11]
				}
				return raw
			})
			f, ctx, u := anchorFollowerPollerFixture(t, endpoint, seeds)
			before := anchorFollowerImage(ctx, t, f.store)
			state, err := anchorOperationOwner(ctx, t, f.s).State()
			if err != nil {
				t.Fatal(err)
			}
			lower, err := state.RetryDue(u.clock.Sample())
			if err != nil {
				t.Fatal(err)
			}
			step, err := u.step(ctx)
			upper, scheduleErr := state.RetryDue(u.clock.Sample())
			if scheduleErr != nil || !step.attempted || reads.Load() != 2 || step.result.branch != sqlite.AnchorPollInvalid {
				t.Fatal("fixture did not reach one original follower receipt", err, scheduleErr)
			}
			if kind == "incomplete" {
				if err != nil || step.acquisitionError == nil || step.next.Before(lower) || step.next.After(upper) || u.stopped != nil {
					t.Fatal("incomplete transport lost accepted retry schedule", step, err)
				}
				if next, err := u.step(ctx); err != nil || next.attempted || !next.next.Equal(step.next) || reads.Load() != 2 {
					t.Fatal("failed acquisition retried without its cooldown", next, err)
				}
			} else {
				var acquisition *anchorAcquisitionFailure
				if err == nil || errors.As(err, &acquisition) || step.acquisitionError != nil || !u.nextAt.IsZero() {
					t.Fatal("complete invalid response became a transport retry", step, err)
				}
				if _, nextErr := u.step(ctx); nextErr != err || reads.Load() != 2 {
					t.Fatal("terminal refusal was recaptured", nextErr)
				}
			}
			if !anchorFollowerSameImage(before, anchorFollowerImage(ctx, t, f.store)) {
				t.Fatal("failed follower observation changed canonical history")
			}
		})
	}
}

func TestAnchorFollowerPollerPromotionDuringNormalReceiptIsTerminal(t *testing.T) {
	interpose := make(chan func() error, 1)
	observed := make(chan error, 1)
	endpoint, seeds, reads := anchorFollowerPollerServer(t, func(_ net.Conn, raw []byte) []byte {
		observed <- (<-interpose)()
		return raw
	})
	f, ctx, u := anchorFollowerPollerFixture(t, endpoint, seeds)
	interpose <- func() error {
		a := f.store.Authority()
		_, err := f.store.Promote(ctx, replication.PromoteRequest{ExpectedTerm: a.Term, ExpectedInstalled: a.Installed, RequestID: "poller-normal-promotion"})
		return err
	}
	step, err := u.step(ctx)
	select {
	case promotionErr := <-observed:
		if promotionErr != nil {
			t.Fatal(promotionErr)
		}
	case <-ctx.Done():
		t.Fatal("normal receipt never reached original promotion")
	}
	if !errors.Is(err, replication.ErrFenced) || !step.attempted || step.acquisitionError != nil || !u.nextAt.IsZero() || reads.Load() != 2 {
		t.Fatal("normal observation adopted promoted authority", step, err)
	}
	if _, nextErr := u.step(ctx); nextErr != err || reads.Load() != 2 {
		t.Fatal("promotion refusal was replayed", nextErr)
	}
}

func TestAnchorFollowerPollerRetainsEpochDuringCooldown(t *testing.T) {
	endpoint, seeds, reads := anchorOperationServer(t, 3, nil)
	f, ctx, u := anchorFollowerPollerFixture(t, endpoint, seeds)
	if _, err := u.step(ctx); err != nil {
		t.Fatal(err)
	}
	term := f.store.Authority().Term
	term.Epoch++
	term.PromotionID = "successor-follower-term"
	if _, err := f.store.ObserveAuthority(ctx, term); err != nil {
		t.Fatal(err)
	}
	if _, err := u.step(ctx); err == nil || reads.Load() != 2 || u.stopped == nil {
		t.Fatal("cooldown adopted a successor follower term", err)
	}
	// Display names are not term identity, but every authority identifier and
	// the local incarnation are. The runtime test above observes actual authority.
	epoch := followerEpoch(f.store.Authority())
	alias := epoch
	alias.term.PrimaryName = "display-only"
	if !epoch.same(alias) {
		t.Fatal("display name replaced authority identity")
	}
	alias.incarnation++
	if epoch.same(alias) {
		t.Fatal("local incarnation was omitted from episode identity")
	}
}

func TestAnchorFollowerPollerRunJoinsSocketAndRuntime(t *testing.T) {
	for _, kind := range []string{"caller-wait", "runtime-wait", "caller-socket", "runtime-socket"} {
		t.Run(kind, func(t *testing.T) {
			entered, closed := make(chan struct{}), make(chan struct{})
			endpoint, seeds, reads := anchorFollowerPollerServer(t, func(conn net.Conn, raw []byte) []byte {
				close(entered)
				if kind == "caller-socket" || kind == "runtime-socket" {
					var one [1]byte
					_, err := conn.Read(one[:])
					if !errors.Is(err, io.EOF) {
						t.Error("cancellation did not close original socket", err)
					}
					close(closed)
					return nil
				}
				return raw
			})
			f, original, _ := anchorFollowerPollerFixture(t, endpoint, seeds)
			ctx, cancel := context.WithCancel(original)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- f.s.runAnchorFollowerPoller(ctx) }()
			joined := false
			t.Cleanup(func() {
				cancel()
				if !joined {
					select {
					case err := <-done:
						t.Logf("follower poller cleanup joined: %v", err)
					case <-time.After(3 * time.Second):
						t.Error("follower poller cleanup failed to join")
					}
				}
			})
			select {
			case <-entered:
			case err := <-done:
				joined = true
				t.Fatal("follower stopped before original socket", err)
			case <-time.After(3 * time.Second):
				t.Fatal("follower did not reach original socket")
			}
			if kind == "caller-wait" || kind == "runtime-wait" {
				// Allow the finite response to enter the scheduler wait. No future
				// receipt or accepted clock value is fabricated.
				select {
				case err := <-done:
					joined = true
					t.Fatal("normal follower unexpectedly stopped", err)
				case <-time.After(100 * time.Millisecond):
				}
			}
			if kind == "runtime-wait" || kind == "runtime-socket" {
				if err := f.s.Close(original); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-done:
				joined = true
				if !errors.Is(err, context.Canceled) || reads.Load() != 2 {
					t.Fatal("follower did not join original cancellation", err, reads.Load())
				}
			case <-time.After(3 * time.Second):
				t.Fatal("follower cancellation failed to join")
			}
			if kind == "caller-socket" || kind == "runtime-socket" {
				select {
				case <-closed:
				case <-time.After(time.Second):
					t.Fatal("original socket remained open after poller returned")
				}
			}
		})
	}
}
