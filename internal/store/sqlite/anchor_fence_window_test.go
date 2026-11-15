package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite/sqlitetest"
)

// anchorFenceWindowLock holds SQLite's write lock from a second connection to
// the store's own database file, so a fence observation stays in flight (its
// authority write waits for the lock) until release is called.
func anchorFenceWindowLock(t *testing.T, s *Store) (release func()) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+s.Path()+"?"+url.Values{"_pragma": {"busy_timeout(10000)"}}.Encode())
	if err != nil {
		t.Fatal(err)
	}
	conn, err := db.Conn(t.Context())
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(t.Context(), `BEGIN IMMEDIATE`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		t.Fatal(err)
	}
	released := false
	release = func() {
		if released {
			return
		}
		released = true
		_, _ = conn.ExecContext(context.Background(), `ROLLBACK`)
		_ = conn.Close()
		_ = db.Close()
	}
	t.Cleanup(release)
	return release
}

// anchorFenceWindowTerm is a higher authenticated term that fences s.
func anchorFenceWindowTerm(s *Store) replication.Term {
	term := s.Authority().Term
	term.Epoch++
	term.PrimaryID, term.PromotionID = "successor", "fence-window"
	return term
}

// anchorFenceWindowCurrent requires the published managed input to stay current
// and readable, and DNS serving authority unblocked, while writes are blocked.
func anchorFenceWindowCurrent(ctx context.Context, t *testing.T, s *Store, input *AnchorResolverInput, want bool) {
	t.Helper()
	_, writes := s.ServingAuthority()
	_, dns := s.DNSServingAuthority()
	if !writes {
		t.Fatal("writes are not blocked during the fence observation")
	}
	if input.Current() != want || dns == want {
		t.Fatalf("during the fence observation: input current %v, DNS blocked %v; want current %v", input.Current(), dns, want)
	}
	if want {
		anchorPublicationTestRead(ctx, t, input)
	} else if _, err := input.CaptureRead(ctx); err == nil {
		t.Fatal("a blocked store admitted a managed read")
	}
}

// A member that observes a fencing term blocks its writers at once and then
// persists the new authority. While that write is in flight it still holds the
// same accepted managed owner and last-good data that its fenced DNS-only role
// will serve, so its published resolver input stays current for DNS output
// (D369). Before this, the write block alone retired the input: every
// validated recursive answer (and every anchor-checked authoritative one)
// failed for as long as the fsync took, SERVFAIL at each conflict detection in
// the enrolled process tests. Writers, protected reads and publication still
// see the store blocked.
func TestAnchorInputStaysCurrentWhileAFenceIsPersisted(t *testing.T) {
	s := openTemp(t, internalOptions)
	_, dormant := anchorLocalTestDormant(t, s)
	anchorTestManaged(t, s, dormant)
	ctx := anchorLocalTestFinite(t.Context(), t)
	input := anchorPublicationTestPublish(ctx, t, s)
	release := anchorFenceWindowLock(t, s)
	done := make(chan error, 1)
	go func() {
		_, err := s.ObserveAuthority(t.Context(), anchorFenceWindowTerm(s))
		done <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for !s.writeBlocked.Load() {
		if time.Now().After(deadline) {
			t.Fatal("the fence observation never blocked writes")
		}
		time.Sleep(time.Millisecond)
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if s.Authority().Mode == replication.Fenced {
			t.Fatal("the fence was published while its write was held")
		}
		anchorFenceWindowCurrent(ctx, t, s, input, true)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a := s.Authority(); a.Mode != replication.Fenced || s.writeBlocked.Load() || s.fenceInFlight.Load() {
		t.Fatal("the fence did not complete", a.Mode, s.writeBlocked.Load(), s.fenceInFlight.Load())
	}
	if _, blocked := s.DNSServingAuthority(); blocked || !input.Current() {
		t.Fatal("the fenced member lost its DNS serving authority or accepted input")
	}
}

// A fence whose persistence fails keeps the store blocked, and DNS output with
// it, exactly as before: the member cannot prove which authority it holds. A
// retry of that observation is not a new transition and does not reopen DNS
// while it waits either. Only a successful retry does.
func TestAnchorInputStaysRetiredAfterAFailedFence(t *testing.T) {
	s := openTemp(t, internalOptions, func(o *Options) { o.BusyTimeout = 200 * time.Millisecond })
	_, dormant := anchorLocalTestDormant(t, s)
	anchorTestManaged(t, s, dormant)
	ctx := anchorLocalTestFinite(t.Context(), t)
	input := anchorPublicationTestPublish(ctx, t, s)
	term := anchorFenceWindowTerm(s)
	release := anchorFenceWindowLock(t, s)
	if _, err := s.ObserveAuthority(t.Context(), term); err == nil {
		t.Fatal("the fence persisted while another connection held the write lock")
	}
	if !s.writeBlocked.Load() || s.fenceInFlight.Load() {
		t.Fatal("a failed fence did not leave the store blocked", s.writeBlocked.Load(), s.fenceInFlight.Load())
	}
	anchorFenceWindowCurrent(ctx, t, s, input, false)
	done := make(chan error, 1)
	go func() {
		_, err := s.ObserveAuthority(t.Context(), term)
		done <- err
	}()
	retry := true
	for retry {
		select {
		case err := <-done:
			if err == nil {
				t.Fatal("the retry persisted while the lock was held")
			}
			retry = false
		default:
			anchorFenceWindowCurrent(ctx, t, s, input, false)
			time.Sleep(10 * time.Millisecond)
		}
	}
	release()
	if _, err := s.ObserveAuthority(t.Context(), term); err != nil {
		t.Fatal(err)
	}
	if a := s.Authority(); a.Mode != replication.Fenced || s.writeBlocked.Load() {
		t.Fatal("the retried fence did not complete", a.Mode)
	}
	if _, blocked := s.DNSServingAuthority(); blocked {
		t.Fatal("the completed fence left DNS blocked")
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatal("fixture lifetime ended during the test")
	}
}

// anchorFenceWindowDivergence opens a static primary and its follower and
// returns the follower, its static input, a validated same-revision image with
// a different digest, the install options that make the follower record a
// divergence and a context for that install. The follower's writer runs
// through hold: it is called before each statement the writer executes, so a
// test can hold or fail the divergence's authority write while it observes
// the store.
func anchorFenceWindowDivergence(t *testing.T, hold func(follower *Store, stmt string)) (*Store, *AnchorStaticInput, context.Context, *replication.Validated, replication.InstallOptions) {
	t.Helper()
	primary := openTemp(t, internalOptions)
	anchorTestBind(t, primary)
	anchorTestDormant(t, primary)
	follower := openTemp(t, internalOptions, func(o *Options) { o.InitialMode = replication.Follower })
	original := follower.w
	follower.w = sqlitetest.WithExecHook(original.Driver(), dsn(follower.Path(), follower.opts.BusyTimeout, true), func(stmt string) { hold(follower, stmt) })
	follower.w.SetMaxOpenConns(1)
	follower.ws = newStmts(follower.w)
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := follower.Enroll(t.Context(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	source, err := follower.BindAnchorLifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	base := anchorTestCopy(t.Context(), t, primary, follower)
	input, err := source.StaticInput()
	if err != nil || !input.Current() {
		t.Fatal("the installed static input is not current", err)
	}
	ctx := anchorTestInstallContext(t.Context(), t, follower)
	different := *base
	different.Payload.Runtime = append(slices.Clone(base.Payload.Runtime), replication.Entry{Key: "foreign/branch", Data: json.RawMessage(`{"selected":true}`)})
	if err := replication.Seal(&different); err != nil {
		t.Fatal(err)
	}
	validated, err := follower.ValidateImage(ctx, different)
	if err != nil {
		t.Fatal(err)
	}
	return follower, input, ctx, validated, replication.InstallOptions{ExpectedTerm: base.Term, ExpectedInstalled: base.Generation()}
}

// anchorFenceWindowAuthorityWrite reports whether stmt is the authority
// write of a divergence (the store already blocks writers then).
func anchorFenceWindowAuthorityWrite(s *Store, stmt string) bool {
	return s.writeBlocked.Load() && strings.HasPrefix(stmt, "INSERT INTO meta(key,value)")
}

// anchorFenceWindowStatic is anchorFenceWindowCurrent for a static input.
func anchorFenceWindowStatic(t *testing.T, s *Store, input *AnchorStaticInput, want bool) {
	t.Helper()
	_, writes := s.ServingAuthority()
	_, dns := s.DNSServingAuthority()
	if !writes {
		t.Fatal("writes are not blocked during the divergence install")
	}
	if input.Current() != want || dns == want {
		t.Fatalf("during the divergence install: input current %v, DNS blocked %v; want current %v", input.Current(), dns, want)
	}
}

// A follower that installs a same-revision image with a different digest
// records a divergence: it blocks writers, then persists the fenced
// authority. While that write is in flight it still serves the same
// last-good data its DNS-only role keeps, so its anchor input stays current
// for DNS output, exactly as for an observed fence (D369).
func TestAnchorInputStaysCurrentWhileADivergenceIsPersisted(t *testing.T) {
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	follower, input, ctx, validated, opts := anchorFenceWindowDivergence(t, func(s *Store, stmt string) {
		if anchorFenceWindowAuthorityWrite(s, stmt) {
			once.Do(func() {
				close(held)
				<-release
			})
		}
	})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	done := make(chan error, 1)
	go func() {
		_, err := follower.InstallReplica(ctx, validated, opts)
		done <- err
	}()
	select {
	case <-held:
	case err := <-done:
		t.Fatal("the install never wrote a divergence", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the divergence install never reached its authority write")
	}
	for end := time.Now().Add(300 * time.Millisecond); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if follower.Authority().Divergence != nil {
			t.Fatal("the divergence was published while its write was held")
		}
		anchorFenceWindowStatic(t, follower, input, true)
	}
	close(release)
	if err := <-done; !errors.Is(err, replication.ErrDivergent) {
		t.Fatal("the held install did not record the divergence", err)
	}
	if a := follower.Authority(); a.Mode != replication.Fenced || a.Divergence == nil || follower.writeBlocked.Load() || follower.fenceInFlight.Load() {
		t.Fatal("the divergence did not complete", a.Mode, follower.writeBlocked.Load(), follower.fenceInFlight.Load())
	}
	if _, blocked := follower.DNSServingAuthority(); blocked {
		t.Fatal("the diverged member lost its DNS serving authority")
	}
}

// A divergence whose authority write fails leaves the store blocked, and DNS
// output with it: the member cannot prove which authority it holds.
func TestAnchorInputStaysRetiredAfterAFailedDivergence(t *testing.T) {
	parent, cancel := context.WithCancel(t.Context())
	defer cancel()
	var failed atomic.Bool
	follower, input, _, validated, opts := anchorFenceWindowDivergence(t, func(s *Store, stmt string) {
		if anchorFenceWindowAuthorityWrite(s, stmt) && failed.CompareAndSwap(false, true) {
			cancel() // the authority write runs on a canceled context and fails
		}
	})
	ctx := anchorTestInstallContext(parent, t, follower)
	if _, err := follower.InstallReplica(ctx, validated, opts); err == nil || errors.Is(err, replication.ErrDivergent) {
		t.Fatal("the divergence persisted although its authority write failed", err)
	}
	if !failed.Load() {
		t.Fatal("the install never reached its authority write")
	}
	// The divergence is published in memory either way (it fences the
	// member); only its durable record failed.
	if !follower.writeBlocked.Load() || follower.fenceInFlight.Load() {
		t.Fatal("a failed divergence did not leave the store blocked", follower.writeBlocked.Load(), follower.fenceInFlight.Load())
	}
	anchorFenceWindowStatic(t, follower, input, false)
}
