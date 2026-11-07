package persistence_test

import (
	"bytes"
	"context"
	"crypto"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/persistence"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

var sharedValidator = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func openStore(t *testing.T, path string, mode replication.Mode, registered bool) *sqlite.Store {
	t.Helper()
	if store.SchemaVersion != 8 {
		t.Fatal("fixture requires the parent's actual schema8 composition")
	}
	opts := sqlite.Options{Identity: "anchor-fixture", InitialMode: mode, Validator: sharedValidator()}
	if registered {
		opts.ValidateReplicaEntry, opts.ValidateReplicaImage = accepted.ValidateEntry, accepted.ValidateImage
	}
	s, err := sqlite.Open(path, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func carry(ctx context.Context, t *testing.T, s *sqlite.Store) context.Context {
	t.Helper()
	original := finite(ctx, t)
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.WithWriterToken(original, token)
}

// finite begins the fixture's original bounded work BEFORE any capture or
// acquisition. It never wraps/rescues a previously captured operation.
func finite(ctx context.Context, t *testing.T) context.Context {
	t.Helper()
	original, cancel := context.WithTimeout(ctx, 10*time.Second)
	t.Cleanup(cancel)
	return original
}

func bind(ctx context.Context, t *testing.T, s *sqlite.Store) {
	t.Helper()
	if _, err := s.BindAnchorLifecycle(ctx); err != nil {
		t.Fatal("bind actual committed owner and local carrier", err)
	}
}

func captureUpdate(ctx context.Context, t *testing.T, s *sqlite.Store, current accepted.Snapshot) *sqlite.AnchorUpdate {
	t.Helper()
	update, err := s.CaptureAnchorUpdate(ctx, current.Version())
	if err != nil {
		t.Fatal("capture original mounted update before acquisition", err)
	}
	return update
}

// runOwnerCommand accepts only a caller's already captured update. Nil is an
// explicit ordinary initialization/no-op boundary, never a managed fallback.
// Adapter-only negative read commands below are separate prewrite API probes.
func runOwnerCommand(ctx context.Context, s *sqlite.Store, update *sqlite.AnchorUpdate, fn func(context.Context, *sqlite.CommandTx) error) error {
	if update != nil {
		_, err := s.AnchorCommand(update, fn)
		return err
	}
	writer, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok {
		return persistence.ErrOriginal
	}
	return s.Command(ctx, writer.Term(), fn)
}

// initializeOwner mirrors the parent's startup composition in the SAME
// original writer command. Managed history is validated unchanged; only Dormant
// may bind migration provenance, including the unchanged-Dormant no-op path.
func initializeOwner(ctx context.Context, tx *sqlite.CommandTx, original persistence.Original) (persistence.Staged, error) {
	staged, err := persistence.InitializeDormant(ctx, tx, original)
	if err != nil {
		return staged, err
	}
	current, err := persistence.Load(ctx, tx)
	if err != nil {
		return staged, err
	}
	if current.Inspect().Phase == accepted.Dormant {
		err = tx.InitializeAnchorLocal(ctx)
	}
	return staged, err
}

// corruptClosed changes only a stopped real schema8 database, preserving its
// schema marker. Public reserved-prefix guards cannot mask the intended load
// or initialization failure, and no proposal is bound before that failure.
func corruptClosed(ctx context.Context, t *testing.T, s *sqlite.Store, path string, change func(context.Context, *sql.Tx) error) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var marker string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&marker); err != nil || marker != "8" {
		t.Fatal("corruption fixture is not actual current schema8", err)
	}
	if err := change(ctx, tx); err != nil {
		t.Fatal(err)
	}
	var after string
	if err := tx.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='schema_version'`).Scan(&after); err != nil || after != marker {
		t.Fatal("controlled corruption changed schema provenance", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedStore(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), sqlite.FileName)
	// The parent's actual schema8 migration creates sentinel + private awaiting
	// provenance. This fixture adds only its independent foreign-owner witness.
	s := openStore(t, path, replication.Standalone, false)
	if !accepted.IsAwaitingOwner(imageOf(t, s).Payload.Runtime) {
		t.Fatal("fresh schema8 Store lacks physical owner sentinel")
	}
	ctx := carry(t.Context(), t, s)
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return tx.PutSection(ctx, accepted.Section, replication.Entry{Key: "foreign/state", Data: json.RawMessage(`{"preserved":true}`)})
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return openStore(t, path, replication.Standalone, true), path
}

func initialize(t *testing.T, s *sqlite.Store) accepted.Snapshot {
	t.Helper()
	ctx := carry(t.Context(), t, s)
	original, err := persistence.CaptureInitialization(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := initializeOwner(ctx, tx, original)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	bind(ctx, t, s) // Only committed owner/local rows can arm the source.
	return load(t, s)
}

func load(t *testing.T, s *sqlite.Store) accepted.Snapshot {
	t.Helper()
	var snap accepted.Snapshot
	if err := s.Command(carry(t.Context(), t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		snap, err = persistence.Load(ctx, tx)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return snap
}

func imageOf(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i
}

func unchanged(t *testing.T, s *sqlite.Store, before *replication.Image) {
	t.Helper()
	after := imageOf(t, s)
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("refused operation changed durable image")
	}
}

type signer struct {
	key     *dns.DNSKEY
	private crypto.Signer
}

func newSigner(t *testing.T) signer {
	t.Helper()
	key := &dns.DNSKEY{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeDNSKEY, Class: dns.ClassINET, Ttl: 3600}, Flags: dns.ZONE | dns.SEP, Protocol: 3, Algorithm: dns.ECDSAP256SHA256}
	private, err := key.Generate(256)
	if err != nil {
		t.Fatal("generate actual root signing key")
	}
	return signer{key, private.(crypto.Signer)}
}

func signed(t *testing.T, key signer, ttl uint32, revoke bool) *dns.Msg {
	t.Helper()
	rr := dns.Copy(key.key).(*dns.DNSKEY)
	rr.Hdr.Ttl = ttl
	if revoke {
		rr.Flags |= dns.REVOKE
	}
	sig := &dns.RRSIG{Hdr: dns.RR_Header{Name: ".", Rrtype: dns.TypeRRSIG, Class: dns.ClassINET, Ttl: ttl}, Algorithm: rr.Algorithm, KeyTag: rr.KeyTag(), SignerName: ".", Inception: uint32(time.Now().Add(-time.Hour).Unix() & 0xffffffff), Expiration: uint32(time.Now().Add(time.Hour).Unix() & 0xffffffff)}
	if err := sig.Sign(key.private, []dns.RR{rr}); err != nil {
		t.Fatal("sign original root response")
	}
	q := new(dns.Msg)
	q.SetQuestion(".", dns.TypeDNSKEY)
	m := new(dns.Msg)
	m.SetReply(q)
	m.Authoritative = true
	m.Answer = []dns.RR{rr, sig}
	return m
}

func newClock(t *testing.T) *trustanchors.SessionClock {
	t.Helper()
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	return clock
}

func capture(ctx context.Context, t *testing.T, snap accepted.Snapshot, clock *trustanchors.SessionClock) persistence.Original {
	t.Helper()
	o, err := persistence.CaptureTransition(ctx, snap.Version(), clock)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

func proposal(t *testing.T, s *sqlite.Store, key signer, clock *trustanchors.SessionClock, ttl uint32) (trustanchors.State, trustanchors.Observation) {
	t.Helper()
	state, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: s.Authority().Term.ClusterID, PolicyID: "explicit-operator-seed", DS: []string{key.key.ToDS(dns.SHA256).String()}}, signed(t, key, ttl, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	return state, observation
}

func bootstrap(t *testing.T, s *sqlite.Store, dormant accepted.Snapshot, key signer, clock *trustanchors.SessionClock) accepted.Snapshot {
	t.Helper()
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, dormant)
	original := capture(ctx, t, dormant, clock) // BEFORE network/proof acquisition.
	state, observation := proposal(t, s, key, clock, 3600)
	var staged persistence.Staged
	if err := runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		staged, err = persistence.Bootstrap(ctx, tx, original, state, observation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// Only now can the version/checkpoint be treated as accepted durable input.
	got := load(t, s)
	if got.Version() != staged.ProposedVersion {
		t.Fatal("committed bootstrap differs from proposal")
	}
	return got
}

func advance(t *testing.T, s *sqlite.Store, current accepted.Snapshot, key signer, clock *trustanchors.SessionClock, revoke bool) accepted.Snapshot {
	t.Helper()
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, current)
	original := capture(ctx, t, current, clock)
	state, err := current.State()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, signed(t, key, 3600, revoke), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	if err := runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, original, observation)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return load(t, s)
}

func waitOriginalExpiry(t *testing.T, observation trustanchors.Observation) {
	t.Helper()
	timer := time.NewTimer(max(0, time.Until(observation.ValidUntil())) + time.Millisecond)
	defer timer.Stop()
	select {
	case <-timer.C:
	case <-t.Context().Done():
		t.Fatal("test canceled before original expiry")
	}
}

func TestSQLiteSentinelOnlyInitializationNoopAndRestart(t *testing.T) {
	s, path := seedStore(t)
	first := initialize(t, s)
	before := imageOf(t, s)
	second := initialize(t, s)
	if second.Version() != first.Version() || first.Inspect().Phase != accepted.Dormant {
		t.Fatal("no-op reinitialized history")
	}
	unchanged(t, s, before)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openStore(t, path, replication.Standalone, true)
	bind(carry(t.Context(), t, s), t, s)
	if load(t, s).Version() != first.Version() {
		t.Fatal("restart minted owner history")
	}
	for _, row := range imageOf(t, s).Payload.Runtime {
		if row.Key == "foreign/state" && !bytes.Equal(row.Data, []byte(`{"preserved":true}`)) {
			t.Fatal("foreign runtime changed")
		}
	}
	if _, err := persistence.CaptureInitialization(t.Context()); !errors.Is(err, persistence.ErrOriginal) {
		t.Fatal("missing original carried writer accepted")
	}
}

func TestSQLiteMissingAndOrphanHistoryRefusesInitialization(t *testing.T) {
	for _, orphan := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "orphan"}[orphan], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), sqlite.FileName)
			s := openStore(t, path, replication.Standalone, false)
			corruptClosed(t.Context(), t, s, path, func(ctx context.Context, tx *sql.Tx) error {
				result, err := tx.ExecContext(ctx, `DELETE FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey)
				if err != nil {
					return err
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatal("fixture did not remove exact physical sentinel", err)
				}
				if orphan {
					_, err := tx.ExecContext(ctx, `INSERT INTO replication_sections(section,key,data) VALUES (?,?,?)`, accepted.Section, accepted.Prefix+"unknown", `{}`)
					return err
				}
				return nil
			})
			s = openStore(t, path, replication.Standalone, false)
			ctx := carry(t.Context(), t, s)
			before := imageOf(t, s)
			original, err := persistence.CaptureInitialization(ctx)
			if err != nil {
				t.Fatal(err)
			}
			entered := false
			err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				entered = true
				_, err := persistence.InitializeDormant(ctx, tx, original)
				return err
			})
			if !entered {
				t.Fatal("initialization fixture failed before intended missing/orphan adapter target", err)
			}
			if err == nil {
				t.Fatal("missing or unknown history reset")
			}
			unchanged(t, s, before)
		})
	}
}

func TestSQLiteBootstrapAdvanceExactEnvelopeAndPureCAS(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	managed := bootstrap(t, s, dormant, key, clock)
	ctx := carry(t.Context(), t, s)
	// Adapter-only prewrite probes below retain deliberately invalid/stale
	// Originals. They do not mutate through an uncaptured managed lane or
	// recapture/rescue a stale mounted update; driver stale refusal is separate.
	original := capture(ctx, t, managed, clock)
	state, err := managed.State()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, signed(t, key, 3600, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	next := advance(t, s, managed, key, clock, false)
	if next.Version().Generation != managed.Version().Generation+1 || next.Version().Pure.Generation != managed.Version().Pure.Generation+1 {
		t.Fatal("both versions did not advance")
	}
	before := imageOf(t, s)
	// Old envelope refuses even though the underlying trust key is unchanged.
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, original, observation)
		return err
	})
	if !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("stale envelope admitted", err)
	}
	unchanged(t, s, before)
	// A freshly supplied envelope cannot rescue the old pure-state observation.
	successor := capture(ctx, t, next, clock)
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, successor, observation)
		return err
	})
	if !errors.Is(err, trustanchors.ErrStale) {
		t.Fatal("successor adopted original observation", err)
	}
	unchanged(t, s, before)
	// Same generation with a changed canonical-root digest also refuses.
	version := next.Version()
	version.Digest = managed.Version().Digest
	wrong, err := persistence.CaptureTransition(ctx, version, clock)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, wrong, observation)
		return err
	})
	if !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("changed envelope digest admitted", err)
	}
	unchanged(t, s, before)
	version = next.Version()
	version.Pure.Digest = managed.Version().Pure.Digest
	wrongPure, err := persistence.CaptureTransition(ctx, version, clock)
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, wrongPure, observation)
		return err
	})
	if !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("pure digest comparison omitted", err)
	}
	unchanged(t, s, before)
}

// The mounted original driver fence is a different boundary from the exact
// adapter-only accepted.ErrCAS checks above. Each operation below is captured
// once before its own proof acquisition; the refused original is never rescued
// or replayed with a new target, token, context or observation.
func TestSQLiteCapturedDriverStaleOwnerRefusesBeforeAdapter(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	managed := bootstrap(t, s, dormant, key, clock)
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, managed)
	original := capture(ctx, t, managed, clock)
	state, err := managed.State()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, signed(t, key, 3600, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	next := advance(t, s, managed, key, clock, false) // Independent admitted effect.
	before := imageOf(t, s)
	called := false
	result, err := s.AnchorCommand(update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		called = true
		_, err := persistence.Advance(ctx, tx, original, observation)
		return err
	})
	if !errors.Is(err, retirement.ErrMismatch) || called || result.Outcome() != sqlite.AnchorRolledBack {
		t.Fatal("mounted stale original crossed the driver cursor fence", err)
	}
	if load(t, s).Version() != next.Version() {
		t.Fatal("stale driver refusal displaced actual accepted successor")
	}
	unchanged(t, s, before)
}

func TestSQLiteOuterFailureNeverAcceptsStagedCheckpoint(t *testing.T) {
	for _, failure := range []string{"callback", "final-source", "original-cancel", "original-deadline"} {
		t.Run(failure, func(t *testing.T) {
			s, _ := seedStore(t)
			dormant := initialize(t, s)
			key, clock := newSigner(t), newClock(t)
			commandCtx := carry(t.Context(), t, s)
			base := commandCtx
			var cancel context.CancelFunc
			if failure == "original-deadline" {
				base, cancel = context.WithTimeout(base, 200*time.Millisecond)
			} else {
				base, cancel = context.WithCancel(base)
			}
			defer cancel()
			original := capture(base, t, dormant, clock)
			update := captureUpdate(commandCtx, t, s, dormant)
			state, observation := proposal(t, s, key, clock, 3600)
			before := imageOf(t, s)
			var staged persistence.Staged
			refusal := errors.New("fixture: original source refused")
			err := runOwnerCommand(commandCtx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
				// All independent transaction work/check registration precedes the
				// adapter's terminal postimage operation, matching its caller contract.
				if err := tx.PutSection(ctx, accepted.Section, replication.Entry{Key: "source/head", Data: json.RawMessage(`{"generation":"1"}`)}); err != nil {
					return err
				}
				if failure == "final-source" {
					if err := tx.RequireCommitCheck(func(context.Context) error { return refusal }); err != nil {
						return err
					}
				}
				var err error
				staged, err = persistence.Bootstrap(ctx, tx, original, state, observation)
				if err != nil {
					return err
				}
				switch failure {
				case "callback":
					return refusal
				case "original-cancel":
					cancel()
				case "original-deadline":
					<-base.Done()
				}
				return nil
			})
			if err == nil || staged.ProposedVersion.Generation != 2 {
				t.Fatal("missing staged result or accepted refused outer command", err)
			}
			if load(t, s).Version() != dormant.Version() {
				t.Fatal("staged checkpoint acquired durable authority after failure")
			}
			unchanged(t, s, before)
		})
	}
}

func TestSQLiteOriginalBootstrapExpiryAtEntryAndFinal(t *testing.T) {
	for _, heldAt := range []string{"entry", "final"} {
		t.Run(heldAt, func(t *testing.T) {
			s, _ := seedStore(t)
			dormant := initialize(t, s)
			key, clock := newSigner(t), newClock(t)
			ctx := carry(t.Context(), t, s)
			update := captureUpdate(ctx, t, s, dormant)
			original := capture(ctx, t, dormant, clock)
			state, observation := proposal(t, s, key, clock, 1)
			before := imageOf(t, s)
			if heldAt == "entry" {
				waitOriginalExpiry(t, observation)
			}
			staged := false
			err := runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
				_, err := persistence.Bootstrap(ctx, tx, original, state, observation)
				if err != nil {
					return err
				}
				staged = true
				if heldAt == "final" {
					waitOriginalExpiry(t, observation)
				}
				return nil
			})
			if !errors.Is(err, trustanchors.ErrExpired) || staged != (heldAt == "final") {
				t.Fatal("original bootstrap expiry was restamped or wrong boundary", err)
			}
			unchanged(t, s, before)
		})
	}
}

func TestSQLiteOriginalAdvanceExpiryAndClockBinding(t *testing.T) {
	s, _ := seedStore(t)
	dormant := initialize(t, s)
	key, clock := newSigner(t), newClock(t)
	managed := bootstrap(t, s, dormant, key, clock)
	ctx := carry(t.Context(), t, s)
	update := captureUpdate(ctx, t, s, managed)
	original := capture(ctx, t, managed, clock)
	state, err := managed.State()
	if err != nil {
		t.Fatal(err)
	}
	observation, err := trustanchors.Verify(state, signed(t, key, 1, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	before := imageOf(t, s)
	err = runOwnerCommand(ctx, s, update, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := persistence.Advance(ctx, tx, original, observation); err != nil {
			return err
		}
		waitOriginalExpiry(t, observation)
		return nil
	})
	if !errors.Is(err, trustanchors.ErrExpired) {
		t.Fatal("held original advance proof gained lifetime", err)
	}
	unchanged(t, s, before)
	otherClock := newClock(t)
	wrong := capture(ctx, t, managed, otherClock)
	fresh, err := trustanchors.Verify(state, signed(t, key, 3600, false), clock.Sample())
	if err != nil {
		t.Fatal(err)
	}
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Advance(ctx, tx, wrong, fresh)
		return err
	})
	if !errors.Is(err, trustanchors.ErrClock) {
		t.Fatal("substituted clock adopted original proof", err)
	}
	unchanged(t, s, before)
}
