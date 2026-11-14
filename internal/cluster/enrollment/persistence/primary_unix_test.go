//go:build unix

package persistence_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment/persistence"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func primaryIdentity(t *testing.T, s *sqlite.Store) *memberidentity.Identity {
	t.Helper()
	id, err := memberidentity.Create(filepath.Join(t.TempDir(), "identity.json"), s.Authority().MemberID)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func primaryCut(t *testing.T, s *sqlite.Store) sqlite.GenerationToken {
	t.Helper()
	_, cut, err := s.ReadServingCandidate(finite(t))
	if err != nil {
		t.Fatal(err)
	}
	return cut
}

func primaryAudit(t *testing.T, s *sqlite.Store) []sqlite.AuditEntry {
	t.Helper()
	entries, err := s.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestEnrollmentPrimaryAtomicCommandRestartAndFollower(t *testing.T) {
	s, path := initialized(t)
	id := primaryIdentity(t, s)
	before := s.Authority()
	cut := primaryCut(t, s)
	ctx := carried(t, s)
	changed, generation := s.AuthorityChanges(), s.GenerationChanges()
	checks := 0
	err := s.Command(ctx, before.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.RequireCommitCheck(func(context.Context) error {
			checks++
			// The original authorization still sees its old memory incarnation.
			if !reflect.DeepEqual(s.Authority(), before) {
				return errors.New("authority published before actual commit")
			}
			return nil
		}); err != nil {
			return err
		}
		if err := tx.StageEnrollmentPrimary(ctx, cut, id, "primary", "127.0.0.1:8082"); err != nil {
			return err
		}
		staged, err := persistence.Load(ctx, tx)
		if err != nil || staged.State() == nil || len(staged.State().Members()) != 1 || tx.Incarnation() != before.Incarnation {
			t.Fatal("command did not retain original writer and staged first member", err)
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.activate", Actor: "operator", Reason: "explicit first primary"})
	})
	if err != nil || checks != 1 {
		t.Fatal("atomic activation failed", checks, err)
	}
	after := s.Authority()
	first := owner(t, s)
	if after.Mode != replication.Primary || after.Incarnation != before.Incarnation+1 || !after.Term.Same(before.Term) || after.WatchLineage == before.WatchLineage || first.State() == nil || first.State().Members()[0].Trust.Fingerprint() != id.Trust().Fingerprint() {
		t.Fatal("membership and primary authority did not commit together")
	}
	committed := image(t, s)
	if committed.Revision != cut.Generation().Revision+1 || len(primaryAudit(t, s)) != 1 {
		t.Fatal("activation did not make exactly one generation and audit event")
	}
	for _, wake := range []<-chan struct{}{changed, generation} {
		select {
		case <-wake:
		default:
			t.Fatal("committed activation did not notify dependents")
		}
	}
	called := false
	if err := s.Command(ctx, before.Term, func(context.Context, *sqlite.CommandTx) error { called = true; return nil }); !errors.Is(err, replication.ErrFenced) || called {
		t.Fatal("old standalone writer survived activation", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, replication.Standalone)
	if !reflect.DeepEqual(s.Authority(), after) || owner(t, s).Digest() != first.Digest() || !image(t, s).Generation().Same(committed.Generation()) || len(primaryAudit(t, s)) != 1 {
		t.Fatal("physical restart lost atomic activation")
	}
	if _, err := s.BindAnchorLifecycle(carried(t, s)); err != nil {
		t.Fatal(err)
	}
	f := open(t, filepath.Join(t.TempDir(), "follower.db"), replication.Follower)
	if err := f.Enroll(finite(t), after.Term); err != nil {
		t.Fatal(err)
	}
	if _, err := f.BindAnchorLifecycle(finite(t)); err != nil {
		t.Fatal(err)
	}
	install, err := f.CaptureAnchorInstall(finite(t))
	if err != nil {
		t.Fatal(err)
	}
	actual := f.Authority()
	v, err := f.ValidateImage(install, *image(t, s))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.InstallReplica(install, v, replication.InstallOptions{ExpectedTerm: actual.Term, ExpectedInstalled: actual.Installed}); err != nil {
		t.Fatal(err)
	}
	if owner(t, f).Digest() != first.Digest() || f.Authority().Mode != replication.Follower {
		t.Fatal("actual follower install lost first member or adopted primary role")
	}
}

func TestEnrollmentPrimaryOriginalRefusalRollsBackOwnerAuthorityAndAudit(t *testing.T) {
	for _, mode := range []string{"authorization", "cancellation", "deadline", "owner-aba", "repeat", "callback", "audit-sql1811", "authority-sql1811"} {
		t.Run(mode, func(t *testing.T) {
			s, path := initialized(t)
			id, cut := primaryIdentity(t, s), primaryCut(t, s)
			before, oldImage := s.Authority(), image(t, s)
			changed, generation := s.AuthorityChanges(), s.GenerationChanges()
			commandCtx := carried(t, s)
			stageCtx, cancel := context.WithCancel(commandCtx)
			defer cancel()
			if mode == "deadline" {
				var end context.CancelFunc
				stageCtx, end = context.WithTimeout(commandCtx, 100*time.Millisecond)
				defer end()
			}
			if mode == "audit-sql1811" || mode == "authority-sql1811" {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = db.Close() }()
				statement := `CREATE TRIGGER activation_refuse BEFORE INSERT ON audit BEGIN SELECT RAISE(ABORT,'activation audit refusal'); END`
				if mode == "authority-sql1811" {
					statement = `CREATE TRIGGER activation_refuse BEFORE UPDATE ON meta WHEN NEW.key='replication_authority' AND json_extract(NEW.value,'$.mode')='primary' BEGIN SELECT RAISE(ABORT,'activation authority refusal'); END`
				}
				if _, err := db.ExecContext(t.Context(), statement); err != nil {
					t.Fatal(err)
				}
			}
			refusal := errors.New("original operator refusal")
			staged := false
			err := s.Command(commandCtx, before.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if err := tx.RequireCommitCheck(func(context.Context) error {
					if mode == "authorization" {
						return refusal
					}
					return nil
				}); err != nil {
					return err
				}
				if err := tx.StageEnrollmentPrimary(stageCtx, cut, id, "primary", "127.0.0.1:8082"); err != nil {
					return err
				}
				staged = true
				if err := tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.activate", Actor: "operator"}); err != nil {
					return err
				}
				switch mode {
				case "cancellation":
					cancel() // The command context remains alive.
				case "deadline":
					<-stageCtx.Done()
				case "callback":
					return refusal
				case "repeat":
					_ = tx.StageEnrollmentPrimary(stageCtx, cut, id, "primary", "127.0.0.1:8082")
				case "owner-aba":
					proposed, err := persistence.Load(ctx, tx)
					if err != nil {
						return err
					}
					rows, err := proposed.Rows()
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
			if !staged || err == nil {
				t.Fatal("refusal fixture did not reach staged activation", err)
			}
			if errors.Is(err, sqlite.ErrEnrollmentCompletionUnknown) {
				t.Fatal("confirmed pre-commit rollback reported unknown completion", err)
			}
			switch mode {
			case "authorization", "callback":
				if !errors.Is(err, refusal) {
					t.Fatal(err)
				}
			case "cancellation":
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "deadline":
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatal(err)
				}
			case "owner-aba", "repeat":
				if !errors.Is(err, replication.ErrCAS) {
					t.Fatal(err)
				}
			default:
				var actual interface{ Code() int }
				if !errors.As(err, &actual) || actual.Code() != 1811 {
					t.Fatal("did not reach actual SQL1811 target", err)
				}
			}
			if !reflect.DeepEqual(s.Authority(), before) || !image(t, s).Generation().Same(oldImage.Generation()) || owner(t, s).State() != nil || len(primaryAudit(t, s)) != 0 {
				t.Fatal("refusal changed owner, authority, image or audit")
			}
			for _, wake := range []<-chan struct{}{changed, generation} {
				select {
				case <-wake:
					t.Fatal("rolled-back activation published a notification")
				default:
				}
			}
		})
	}
}

func TestEnrollmentPrimaryRejectsChangedOrInvalidOriginal(t *testing.T) {
	for _, mode := range []string{"foreign-cut", "stale-cut", "nil-identity", "foreign-identity", "already-enrolled", "already-primary", "infinite", "excessive-lifetime", "earlier-mutation"} {
		t.Run(mode, func(t *testing.T) {
			s, _ := initialized(t)
			if mode == "already-enrolled" {
				s, _ = active(t)
			}
			id := primaryIdentity(t, s)
			if mode == "already-primary" {
				if _, err := s.EnablePrimary(finite(t), image(t, s).Generation()); err != nil {
					t.Fatal(err)
				}
			}
			cut := primaryCut(t, s)
			if mode == "foreign-cut" {
				other, _ := initialized(t)
				cut = primaryCut(t, other)
			}
			if mode == "stale-cut" {
				if err := s.Command(carried(t, s), s.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					return tx.PutSection(ctx, "runtime", replication.Entry{Key: "independent", Data: []byte(`{}`)})
				}); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "nil-identity" {
				id = nil
			}
			if mode == "foreign-identity" {
				var err error
				id, err = memberidentity.Create(filepath.Join(t.TempDir(), "foreign.json"), uuid.NewString())
				if err != nil {
					t.Fatal(err)
				}
			}
			ctx := carried(t, s)
			if mode == "infinite" || mode == "excessive-lifetime" {
				token, err := s.WriterToken()
				if err != nil {
					t.Fatal(err)
				}
				ctx = sqlite.WithWriterToken(context.Background(), token)
				if mode == "excessive-lifetime" {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, time.Minute)
					defer cancel()
				}
			}
			before, oldImage := s.Authority(), image(t, s)
			err := s.Command(ctx, before.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
				if mode == "earlier-mutation" {
					if err := tx.PutSection(ctx, "runtime", replication.Entry{Key: "earlier", Data: []byte(`{}`)}); err != nil {
						return err
					}
				}
				_ = tx.StageEnrollmentPrimary(ctx, cut, id, "primary", "127.0.0.1:8082")
				return nil // Ignored admission failure still poisons the whole command.
			})
			if err == nil || !reflect.DeepEqual(s.Authority(), before) || !image(t, s).Generation().Same(oldImage.Generation()) {
				t.Fatal("invalid activation or ignored refusal changed durable state", err)
			}
		})
	}
}

func TestEnrollmentPrimaryUnknownCommitFencesOldWriter(t *testing.T) {
	s, path := initialized(t)
	id, cut := primaryIdentity(t, s), primaryCut(t, s)
	before, oldImage := s.Authority(), image(t, s)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	for _, statement := range []string{
		`CREATE TABLE enrollment_parent(id INTEGER PRIMARY KEY)`,
		`CREATE TABLE enrollment_child(parent INTEGER REFERENCES enrollment_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
		`CREATE TRIGGER enrollment_unknown AFTER UPDATE ON meta WHEN NEW.key='replication_authority' AND json_extract(NEW.value,'$.mode')='primary' BEGIN INSERT INTO enrollment_child(parent) VALUES (1); END`,
	} {
		if _, err := db.ExecContext(t.Context(), statement); err != nil {
			t.Fatal(err)
		}
	}
	wake := s.AuthorityChanges()
	err = s.Command(carried(t, s), before.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := tx.StageEnrollmentPrimary(ctx, cut, id, "primary", "127.0.0.1:8082"); err != nil {
			return err
		}
		return tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "cluster.activate", Actor: "operator"})
	})
	var actual interface{ Code() int }
	if !errors.As(err, &actual) || actual.Code() != 787 || !errors.Is(err, sql.ErrTxDone) || !errors.Is(err, sqlite.ErrEnrollmentCompletionUnknown) {
		t.Fatal("did not exercise actual deferred commit failure", err)
	}
	if _, err := s.WriterToken(); !errors.Is(err, replication.ErrFenced) {
		t.Fatal("ambiguous completion left old writer usable", err)
	}
	if _, blocked := s.ServingAuthority(); !blocked {
		t.Fatal("ambiguous completion left serving authority usable")
	}
	select {
	case <-wake:
	default:
		t.Fatal("ambiguous completion did not wake original writer")
	}
	if !reflect.DeepEqual(s.Authority(), before) || !image(t, s).Generation().Same(oldImage.Generation()) || len(primaryAudit(t, s)) != 0 {
		t.Fatal("failed SQL fixture published its candidate")
	}
	// Observed fixture rows are not an in-process rollback receipt. Only a real
	// close/open may read the durable authority again after uncertain completion.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = open(t, path, replication.Standalone)
	if !reflect.DeepEqual(s.Authority(), before) || owner(t, s).State() != nil {
		t.Fatal("restart did not retain actual atomic SQL state")
	}
}
