//go:build unix

package persistence_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/runtimeowners"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

var validation = sync.OnceValue(func() *validator.Validator {
	v, err := validator.New()
	if err != nil {
		panic(err)
	}
	return v
})

func open(t *testing.T, path string, mode replication.Mode) *sqlite.Store {
	t.Helper()
	scheme, err := runtimekinds.NewScheme()
	if err != nil {
		t.Fatal(err)
	}
	var kinds []store.Kind
	for _, k := range runtimekinds.Private() {
		kinds = append(kinds, store.Kind(k))
	}
	s, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: kinds, Validator: validation(), InitialMode: mode, ValidateReplicaEntry: runtimeowners.ValidateEntry, ValidateReplicaImage: runtimeowners.ValidateImage})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func finite(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}
func carried(t *testing.T, s *sqlite.Store) context.Context {
	t.Helper()
	token, err := s.WriterToken()
	if err != nil {
		t.Fatal(err)
	}
	return sqlite.WithWriterToken(finite(t), token)
}
func image(t *testing.T, s *sqlite.Store) *replication.Image {
	t.Helper()
	i, err := s.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	return i
}
func owner(t *testing.T, s *sqlite.Store) accepted.Owner {
	t.Helper()
	i := image(t, s)
	o, err := accepted.DecodeRows(i.Payload.Membership, i.Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func initialized(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s := open(t, path, replication.Standalone)
	ctx := carried(t, s)
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return runtimeowners.Initialize(ctx, tx, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.BindAnchorLifecycle(carried(t, s)); err != nil {
		t.Fatal(err)
	}
	return s, path
}
func original(t *testing.T, s *sqlite.Store, check func() error) (context.Context, *persistence.Original) {
	t.Helper()
	ctx := carried(t, s)
	o, err := persistence.Capture(ctx, owner(t, s), check)
	if err != nil {
		t.Fatal(err)
	}
	return ctx, o
}
func active(t *testing.T) (*sqlite.Store, string) {
	t.Helper()
	s, path := initialized(t)
	i, err := memberidentity.Create(filepath.Join(t.TempDir(), "identity.json"), s.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	ctx, o := original(t, s, func() error { return nil })
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Activate(ctx, tx, o, enrollment.Member{ID: i.Trust().MemberID(), Name: "primary", Address: "127.0.0.1:8082", Trust: i.Trust()})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, path
}
func invitation(t *testing.T, s *sqlite.Store) (enrollment.Proposal, string) {
	t.Helper()
	return invitationWithLifetime(t, s, time.Minute)
}

func invitationWithLifetime(t *testing.T, s *sqlite.Store, lifetime time.Duration) (enrollment.Proposal, string) {
	t.Helper()
	ctx, o := original(t, s, func() error { return nil })
	var invite enrollment.Invitation
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		_, invite, err = persistence.Issue(ctx, tx, o, "follower", "127.0.0.2:8082", time.Now().Add(lifetime))
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	i, err := memberidentity.Create(filepath.Join(t.TempDir(), "identity.json"), uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	p, err := invite.Propose(i)
	if err != nil {
		t.Fatal(err)
	}
	return p, i.Trust().MemberID()
}
func accept(t *testing.T, s *sqlite.Store, p enrollment.Proposal) error {
	t.Helper()
	ctx, o := original(t, s, func() error { return nil })
	return s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Accept(ctx, tx, o, p)
		return err
	})
}

func TestEnrollmentCommandFinalRefusalRollsBackBothEffects(t *testing.T) {
	for _, stage := range []string{"authorization", "original-cancellation", "postimage-aba", "sql1811"} {
		t.Run(stage, func(t *testing.T) {
			s, path := active(t)
			p, _ := invitation(t, s)
			before := image(t, s)
			wake := s.GenerationChanges()
			denied := false
			refusal := errors.New("original operator refused")
			commandCtx := carried(t, s)
			originalCtx, cancel := context.WithCancel(commandCtx)
			defer cancel()
			o, err := persistence.Capture(originalCtx, owner(t, s), func() error {
				if denied {
					return refusal
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			var db *sql.DB
			if stage == "sql1811" {
				db, err = sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				if _, err := db.ExecContext(t.Context(), `CREATE TRIGGER enrollment_refuse BEFORE UPDATE ON replication_sections WHEN NEW.section='membership' AND NEW.key='enrollment/root' BEGIN SELECT RAISE(ABORT,'enrollment fixture refusal'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = s.Command(commandCtx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if _, err := persistence.Accept(ctx, tx, o, p); err != nil {
					return err
				}
				switch stage {
				case "authorization":
					denied = true
				case "original-cancellation":
					cancel()
				case "postimage-aba":
					current, err := persistence.Load(ctx, tx)
					if err != nil {
						return err
					}
					rows, err := current.Rows()
					if err != nil {
						return err
					}
					if err := tx.DeleteSection(ctx, accepted.Section, accepted.RootKey); err != nil {
						return err
					}
					return tx.PutSection(ctx, accepted.Section, rows[0])
				}
				return nil
			})
			switch stage {
			case "authorization":
				if !errors.Is(err, refusal) {
					t.Fatal("late authorization granted", err)
				}
			case "original-cancellation":
				if !errors.Is(err, context.Canceled) {
					t.Fatal("original cancellation granted", err)
				}
			case "postimage-aba":
				if !errors.Is(err, replication.ErrCAS) {
					t.Fatal("postimage ABA granted", err)
				}
			case "sql1811":
				var target interface{ Code() int }
				if !errors.As(err, &target) || target.Code() != 1811 {
					t.Fatal("did not reach actual SQL1811", err)
				}
			}
			if !before.Generation().Same(image(t, s).Generation()) || len(owner(t, s).State().Members()) != 1 {
				t.Fatal("failed admission changed either effect")
			}
			select {
			case <-wake:
				t.Fatal("rollback notified a committed generation")
			default:
			}
			if db != nil {
				if _, err := db.ExecContext(t.Context(), `DROP TRIGGER enrollment_refuse`); err != nil {
					t.Fatal(err)
				}
			}
			if err := accept(t, s, p); err != nil {
				t.Fatal("rollback consumed original invitation", err)
			}
			if err := accept(t, s, p); !errors.Is(err, enrollment.ErrRefused) {
				t.Fatal("committed invitation replayed", err)
			}
		})
	}
}

func TestEnrollmentOriginalOwnerCASAndRawHistoryRemoval(t *testing.T) {
	s, _ := active(t)
	p, _ := invitation(t, s)
	ctx, old := original(t, s, func() error { return nil })
	if err := accept(t, s, p); err != nil {
		t.Fatal(err)
	}
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Revoke(ctx, tx, old, s.Authority().MemberID)
		return err
	})
	if !errors.Is(err, replication.ErrCAS) {
		t.Fatal("stale owner admitted", err)
	}
	before := image(t, s)
	for _, remove := range []bool{false, true} {
		err = s.Command(carried(t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if remove {
				return tx.DeleteSection(ctx, accepted.Section, accepted.RootKey)
			}
			dormant, err := accepted.Dormant(tx.Authority().ClusterID)
			if err != nil {
				return err
			}
			rows, err := dormant.Rows()
			if err != nil {
				return err
			}
			return tx.PutSection(ctx, accepted.Section, rows[0])
		})
		if !errors.Is(err, accepted.ErrOwner) || !before.Generation().Same(image(t, s).Generation()) {
			t.Fatal("raw command erased membership history", err)
		}
	}
	if _, err := s.ApplyCluster(carried(t, s), nil, store.ApplyOptions{}, []sqlite.SectionChange{{Section: accepted.Section, Key: accepted.RootKey, Delete: true}}); !errors.Is(err, replication.ErrInvalid) {
		t.Fatal("legacy bulk lane erased owner", err)
	}
}

func TestEnrollmentPhysicalRestartFollowerInstallAndResurrectionRefusal(t *testing.T) {
	s, path := active(t)
	proposal, id := invitation(t, s)
	if err := accept(t, s, proposal); err != nil {
		t.Fatal(err)
	}
	joined := owner(t, s)
	ctx, o := original(t, s, func() error { return nil })
	if err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := persistence.Revoke(ctx, tx, o, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	revoked := owner(t, s)
	before := image(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, replication.Standalone)
	if _, err := s.BindAnchorLifecycle(carried(t, s)); err != nil {
		t.Fatal(err)
	}
	if owner(t, s).Digest() != revoked.Digest() || !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("restart lost exact committed history")
	}
	if err := accept(t, s, proposal); !errors.Is(err, enrollment.ErrRefused) {
		t.Fatal("restart reset consumption", err)
	}
	fpath := filepath.Join(t.TempDir(), "state.db")
	f := open(t, fpath, replication.Follower)
	if err := f.Enroll(finite(t), s.Authority().Term); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BindAnchorLifecycle(finite(t)); err != nil {
		t.Fatal(err)
	}
	install := func(acquire func() *replication.Image) error {
		ctx, err := f.CaptureAnchorInstall(finite(t))
		if err != nil {
			return err
		}
		actual := f.Authority()
		source := acquire()
		validated, err := f.ValidateImage(ctx, *source)
		if err != nil {
			return err
		}
		_, err = f.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: actual.Term, ExpectedInstalled: actual.Installed})
		return err
	}
	if err := install(func() *replication.Image { return image(t, s) }); err != nil {
		t.Fatal(err)
	}
	followerBefore := image(t, f)
	if owner(t, f).Digest() != revoked.Digest() {
		t.Fatal("follower lost revoked member")
	}
	// A freshly sealed, higher revision cannot rewrite a revoked identity into
	// its prior active state. It passes complete structural image validation.
	if err := install(func() *replication.Image {
		forged := image(t, s)
		forged.Revision++
		rows, err := joined.Rows()
		if err != nil {
			t.Fatal(err)
		}
		for n, row := range forged.Payload.Membership {
			if row.Key == accepted.RootKey {
				forged.Payload.Membership[n] = rows[0]
			}
		}
		if err := replication.Seal(forged); err != nil {
			t.Fatal(err)
		}
		return forged
	}); !errors.Is(err, accepted.ErrOwner) {
		t.Fatal("higher image resurrected member", err)
	}
	if !followerBefore.Generation().Same(image(t, f).Generation()) {
		t.Fatal("refused image changed actual follower")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	f = open(t, fpath, replication.Follower)
	if _, err := f.BindAnchorLifecycle(finite(t)); err != nil {
		t.Fatal(err)
	}
	if owner(t, f).Digest() != revoked.Digest() {
		t.Fatal("follower physical restart lost revocation")
	}
	raw, err := owner(t, f).State().Encode()
	if err != nil {
		t.Fatal(err)
	}
	want, err := revoked.State().Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, want) {
		t.Fatal("follower rewrote canonical enrollment state")
	}
	// Actual explicit conflict resolution must preserve the same local history.
	otherTerm := s.Authority().Term
	otherTerm.PrimaryID, otherTerm.PromotionID = uuid.NewString(), uuid.NewString()
	if _, err := f.ObserveAuthority(finite(t), otherTerm); !errors.Is(err, replication.ErrPromotionConflict) {
		t.Fatal("fixture did not enter actual conflict", err)
	}
	resolutionCtx, err := f.CaptureAnchorInstall(finite(t))
	if err != nil {
		t.Fatal(err)
	}
	actual := f.Authority()
	conflicting := image(t, s)
	conflicting.Term = otherTerm
	joinedRows, err := joined.Rows()
	if err != nil {
		t.Fatal(err)
	}
	for n, row := range conflicting.Payload.Membership {
		if row.Key == accepted.RootKey {
			conflicting.Payload.Membership[n] = joinedRows[0]
		}
	}
	if err := replication.Seal(conflicting); err != nil {
		t.Fatal(err)
	}
	validated, err := f.ValidateImage(resolutionCtx, *conflicting)
	if err != nil {
		t.Fatal(err)
	}
	beforeResolution := image(t, f)
	_, err = f.ResolveConflict(resolutionCtx, validated, replication.ResolveRequest{ExpectedTerm: actual.Term, ExpectedConflict: actual.Conflict, ExpectedInstalled: beforeResolution.Generation(), RequestID: "refuse-resurrection"})
	if !errors.Is(err, accepted.ErrOwner) {
		t.Fatal("resolution replaced permanent member history", err)
	}
	if !reflect.DeepEqual(actual, f.Authority()) || !beforeResolution.Generation().Same(image(t, f).Generation()) {
		t.Fatal("refused resolution partly committed")
	}
	if _, err := f.ReadQuarantined(t.Context(), beforeResolution.Generation()); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("refused resolution leaked quarantine", err)
	}
}

func TestEnrollmentInvitationExpiryAtActualCommitRollsBack(t *testing.T) {
	s, _ := active(t)
	p, _ := invitationWithLifetime(t, s, time.Second)
	before := image(t, s)
	ctx, o := original(t, s, func() error { return nil })
	entered := false
	err := s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if _, err := persistence.Accept(ctx, tx, o, p); err != nil {
			return err
		}
		entered = true
		time.Sleep(1100 * time.Millisecond)
		return nil
	})
	if !entered || !errors.Is(err, enrollment.ErrRefused) {
		t.Fatal("did not refuse the originally admitted invitation at final expiry", err)
	}
	if !before.Generation().Same(image(t, s).Generation()) || len(owner(t, s).State().Members()) != 1 {
		t.Fatal("expired final check partly consumed invitation")
	}
}

func TestEnrollmentOriginalCopiesCannotRepeatAnUnchangedCommand(t *testing.T) {
	s, _ := active(t)
	ctx, original := original(t, s, func() error { return nil })
	copied := *original
	before := image(t, s)
	prune := func(o *persistence.Original) error {
		return s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := persistence.PruneInvitations(ctx, tx, o)
			return err
		})
	}
	if err := prune(original); err != nil {
		t.Fatal(err)
	}
	if !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("fixture did not perform a genuine unchanged command")
	}
	if err := prune(&copied); !errors.Is(err, enrollment.ErrRefused) {
		t.Fatal("copy minted a second original attempt", err)
	}
	if !before.Generation().Same(image(t, s).Generation()) {
		t.Fatal("refused copy changed owner")
	}
}

func TestEnrollmentCurrentSchemaMissingAndCorruptOwnerNeverRepairs(t *testing.T) {
	for _, mode := range []string{"missing", "malformed", "orphan", "oversized"} {
		t.Run(mode, func(t *testing.T) {
			s, path := active(t)
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = db.Close() }()
			switch mode {
			case "missing":
				_, err = db.ExecContext(t.Context(), `DELETE FROM replication_sections WHERE section=? AND key=?`, accepted.Section, accepted.RootKey)
			case "malformed":
				_, err = db.ExecContext(t.Context(), `UPDATE replication_sections SET data='{}' WHERE section=? AND key=?`, accepted.Section, accepted.RootKey)
			case "orphan":
				_, err = db.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES(?,?,?)`, accepted.Section, accepted.Prefix+"orphan", `{}`)
			case "oversized":
				_, err = db.ExecContext(t.Context(), `UPDATE replication_sections SET data=? WHERE section=? AND key=?`, string(bytes.Repeat([]byte("x"), accepted.MaxRowBytes+1)), accepted.Section, accepted.RootKey)
			}
			if err != nil {
				t.Fatal(err)
			}
			read := func() []string {
				rows, err := db.QueryContext(t.Context(), `SELECT json_array(section,key,data) FROM replication_sections ORDER BY section,key`)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = rows.Close() }()
				var out []string
				for rows.Next() {
					var raw string
					if err := rows.Scan(&raw); err != nil {
						t.Fatal(err)
					}
					out = append(out, raw)
				}
				if err := rows.Err(); err != nil {
					t.Fatal(err)
				}
				return out
			}
			before := read()
			s = open(t, path, replication.Standalone)
			entered := false
			err = s.Command(carried(t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				entered = true
				return runtimeowners.Initialize(ctx, tx, time.Now())
			})
			if !entered || err == nil {
				t.Fatal("current-schema corruption did not reach and refuse owner initialization", err)
			}
			var version string
			if err := db.QueryRowContext(t.Context(), `SELECT value FROM meta WHERE key='schema_version'`).Scan(&version); err != nil || version != strconv.Itoa(store.SchemaVersion) {
				t.Fatal("fixture changed the actual schema marker", err)
			}
			if !reflect.DeepEqual(before, read()) {
				t.Fatal("startup repaired or partially changed corrupt history")
			}
		})
	}
}
