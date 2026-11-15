package sqlite

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	memberowner "github.com/robertherbaugh/kudurru/internal/cluster/enrollment/accepted"
	"github.com/robertherbaugh/kudurru/internal/cluster/memberidentity"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

func TestCreateJoinReceiverRequiresOriginalLifetimeAndNewFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	if _, err := CreateJoinReceiver(t.Context(), path, Options{}); err == nil {
		t.Fatal("unbounded initialization accepted")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	cancel()
	if _, err := CreateJoinReceiver(ctx, path, Options{}); err == nil {
		t.Fatal("canceled initialization accepted")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("refused lifetime created a database")
	}
	ctx, cancel = context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	s, err := CreateJoinReceiver(ctx, path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if s.Authority().Mode != replication.Unenrolled {
		t.Fatal("new receiver gained writer authority")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// #nosec G304 -- exact fixture-created database under t.TempDir; compare preserved bytes
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CreateJoinReceiver(ctx, path, Options{}); !errors.Is(err, os.ErrExist) {
		t.Fatal("existing database was opened", err)
	}
	// #nosec G304 -- same fixture path after exclusive-create refusal
	after, err := os.ReadFile(path)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("existing database changed", err)
	}
}

// The primary's membership is fixture administration. Receiver admission uses
// the actual public capture/install and raw SQL boundaries; full runtime/TLS
// acceptance is exercised by cmd/kudurru's separate join tests.
func initialJoinPair(t *testing.T) (*Store, *Store, enrollment.Invitation, *memberidentity.Identity) {
	t.Helper()
	p := openTemp(t, internalOptions, func(o *Options) { o.InitialMode = replication.Primary; o.Identity = "primary" })
	anchorTestDormant(t, p)
	f := openTemp(t, internalOptions, func(o *Options) { o.InitialMode = replication.Unenrolled })
	if _, err := f.BindAnchorLifecycle(t.Context()); err != nil {
		t.Fatal(err)
	}
	identity := func(s *Store) *memberidentity.Identity {
		t.Helper()
		i, err := memberidentity.Create(filepath.Join(t.TempDir(), "identity.json"), s.Authority().MemberID)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	primary, local := identity(p), identity(f)
	write := func(owner memberowner.Owner) {
		t.Helper()
		rows, err := owner.Rows()
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Command(t.Context(), p.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
			return tx.PutSection(ctx, memberowner.Section, rows[0])
		}); err != nil {
			t.Fatal(err)
		}
	}
	dormant, err := memberowner.Dormant(p.Authority().Term.ClusterID)
	if err != nil {
		t.Fatal(err)
	}
	write(dormant)
	state, err := enrollment.Initialize(p.Authority().Term.ClusterID, enrollment.Member{ID: primary.Trust().MemberID(), Name: "primary", Address: "127.0.0.1:8082", Trust: primary.Trust()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	term := p.Authority().Term
	issuer := enrollment.Issuer{MemberID: term.PrimaryID, Epoch: term.Epoch, PromotionID: term.PromotionID}
	state, inv, err := state.Issue(issuer, "receiver", "127.0.0.2:8082", time.Now(), time.Now().Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	proposal, err := inv.Propose(local)
	if err != nil {
		t.Fatal(err)
	}
	state, err = state.Accept(proposal, issuer, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	owner, err := memberowner.Enrolled(state)
	if err != nil {
		t.Fatal(err)
	}
	write(owner)
	return p, f, inv, local
}

func TestInitialJoinOriginalCutAndSQLFailureAtomicity(t *testing.T) {
	for _, mode := range []string{"success", "canceled", "interposed", "rollback1811", "commit787", "occupied", "missing-counter", "queue-deadline"} {
		t.Run(mode, func(t *testing.T) {
			p, f, inv, local := initialJoinPair(t)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			if mode == "queue-deadline" {
				ctx, cancel = context.WithTimeout(t.Context(), time.Second)
				defer cancel()
			}
			before := f.Authority()
			source := f.anchors.Load()
			cursor := source.cursor.Load()
			carrier := cursor.local.raw
			var statements []string
			switch mode {
			case "rollback1811":
				statements = []string{`CREATE TEMP TRIGGER join_abort BEFORE INSERT ON main.replication_sections BEGIN SELECT RAISE(ABORT,'join fixture refusal'); END`}
			case "commit787":
				statements = []string{`CREATE TEMP TABLE join_parent(id INTEGER PRIMARY KEY)`, `CREATE TEMP TABLE join_child(parent INTEGER REFERENCES join_parent(id) DEFERRABLE INITIALLY DEFERRED)`, `CREATE TEMP TRIGGER join_unknown AFTER INSERT ON main.replication_sections BEGIN INSERT INTO join_child(parent) VALUES (1); END`}
			case "occupied":
				statements = []string{`INSERT INTO replication_sections(section,key,data) VALUES('dedup','foreign-history','{}')`}
			case "missing-counter":
				statements = []string{`DELETE FROM meta WHERE key='resource_version'`}
			}
			for _, statement := range statements {
				if _, err := f.w.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			original, err := f.CaptureInitialJoin(ctx, inv, local.Trust())
			if mode == "occupied" || mode == "missing-counter" {
				if err == nil {
					t.Fatal("nonpristine receiver admitted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			image, err := p.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			validated, err := f.ValidateImage(ctx, *image)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "canceled":
				cancel()
			case "interposed":
				if err := f.Enroll(ctx, p.Authority().Term); err != nil {
					t.Fatal(err)
				}
				before = f.Authority()
			case "queue-deadline":
				f.wmu.Lock()
				defer f.wmu.Unlock()
			}
			_, err = f.InstallJoinedReplica(original, validated)
			if mode == "success" {
				if err != nil || f.Authority().Mode != replication.Follower || !f.Authority().Installed.Same(image.Generation()) || source.cursor.Load() == cursor {
					t.Fatal("atomic first installation failed", err)
				}
				if _, err := f.InstallJoinedReplica(original, validated); err == nil {
					t.Fatal("join request reused")
				}
				return
			}
			if err == nil {
				t.Fatal("refused join succeeded")
			}
			if mode == "rollback1811" || mode == "commit787" {
				var coded interface{ Code() int }
				want := 1811
				if mode == "commit787" {
					want = 787
				}
				if !errors.As(err, &coded) || coded.Code() != want {
					t.Fatal("did not reach actual SQL target", err)
				}
			}
			if mode == "queue-deadline" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("original lock wait did not expire", err)
			}
			if !reflect.DeepEqual(before, f.Authority()) || source.cursor.Load() != cursor || cursor.local.raw != carrier {
				t.Fatal("failed first image changed authority or carrier")
			}
			if mode == "commit787" && (!source.closed.Load() || !f.anchorBlocked.Load()) {
				t.Fatal("unknown SQL completion left admission open")
			}
		})
	}
}
