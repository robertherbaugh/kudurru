package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// The fixture asserts actual schema8 Open/migration provenance without
// reconstructing a carrier or weakening the original admission assertions.
// Raw drivers deliberately omit complete-owner validators; none of these
// sentinel cases represents a complete runtime image or serving permission.
func anchorAwaitingTestPair(t *testing.T, mutate ...func(*Options)) (*Store, *Store, *AnchorSource) {
	t.Helper()
	primary := openTemp(t, internalOptions)
	anchorLocalTestAwaiting(t, primary)
	options := []func(*Options){internalOptions, func(o *Options) { o.InitialMode = replication.Follower }}
	options = append(options, mutate...)
	follower := openTemp(t, options...)
	if err := follower.Enroll(t.Context(), primary.Authority().Term); err != nil {
		t.Fatal(err)
	}
	anchorLocalTestAwaiting(t, follower)
	source, err := follower.BindAnchorLifecycle(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !anchorAwaitingInstallCursor(source.cursor.Load()) {
		t.Fatal("raw fixture lacks exact migration provenance")
	}
	return primary, follower, source
}

func anchorAwaitingTestImage(ctx context.Context, t *testing.T, primary, receiver *Store) *replication.Validated {
	t.Helper()
	// The caller captures BEFORE this original source acquisition/selection.
	image, err := primary.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := receiver.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal("raw fixture refused before targeted admission", err)
	}
	return validated
}

func anchorAwaitingTestOptions(ctx context.Context, t *testing.T) replication.InstallOptions {
	t.Helper()
	cell, ok := ctx.Value(anchorInstallKey{}).(*anchorInstall)
	if !ok {
		t.Fatal("fixture lost original install capture")
	}
	var original replication.Authority
	if err := json.Unmarshal([]byte(cell.authority), &original); err != nil {
		t.Fatal("fixture lost captured original authority", err)
	}
	return replication.InstallOptions{ExpectedTerm: original.Term, ExpectedInstalled: cell.installed}
}

func anchorAwaitingTestUnchanged(t *testing.T, s *Store, source *AnchorSource, cursor *anchorCursor, authority, carrier string) {
	t.Helper()
	if source.cursor.Load() != cursor || anchorAuthority(s.Authority()) != authority || anchorLocalTestRead(t, s).raw != carrier {
		t.Fatal("refused/no-op install changed original source, authority or carrier")
	}
}

func anchorAwaitingTestNoGrant(t *testing.T, source *AnchorSource, before *anchorCursor) {
	t.Helper()
	current := source.cursor.Load()
	if current != before || !anchorAwaitingInstallCursor(current) || current.owner.Version() != (accepted.Version{}) || current.gate != nil || current.mark != (retirement.Mark{}) {
		t.Fatal("raw sentinel install minted owner/history/gate or changed local provenance")
	}
}

func TestAnchorAwaitingInstallFirstLaterAndNoop(t *testing.T) {
	primary, follower, source := anchorAwaitingTestPair(t)
	cursor := source.cursor.Load()
	carrier := cursor.local.raw
	for _, phase := range []string{"first", "later", "noop"} {
		t.Run(phase, func(t *testing.T) {
			if phase == "later" {
				mustApply(t, primary, zone("later", "later.example."), store.ManagerAPI)
				if _, err := primary.ApplyCluster(t.Context(), nil, store.ApplyOptions{}, []SectionChange{{Section: "runtime", Key: "foreign/witness", Data: json.RawMessage(`{"intact":true}`)}}); err != nil {
					t.Fatal(err)
				}
			}
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			cell := ctx.Value(anchorInstallKey{}).(*anchorInstall)
			originalDeadline, ok := cell.original.Deadline()
			gotDeadline, gotOK := ctx.Deadline()
			if !ok || !gotOK || !originalDeadline.Equal(gotDeadline) || cell.original.Done() != ctx.Done() {
				t.Fatal("capture replaced original acquisition lifetime")
			}
			validated := anchorAwaitingTestImage(ctx, t, primary, follower)
			beforeAuthority := anchorAuthority(follower.Authority())
			generation, err := follower.InstallReplica(ctx, validated, anchorAwaitingTestOptions(ctx, t))
			if err != nil || !generation.Same(validated.Image().Generation()) {
				t.Fatal("captured raw install did not retain exact source image", err)
			}
			actual := imageOf(t, follower)
			if !actual.Generation().Same(generation) || !reflect.DeepEqual(actual.Payload, validated.Image().Payload) {
				t.Fatal("raw install converted received canonical bytes")
			}
			anchorAwaitingTestNoGrant(t, source, cursor)
			if carrier != anchorLocalTestRead(t, follower).raw {
				t.Fatal("replication copied or advanced receiver-local provenance")
			}
			if phase == "noop" && beforeAuthority != anchorAuthority(follower.Authority()) {
				t.Fatal("exact raw no-op changed authority")
			}
			if _, err := follower.InstallReplica(ctx, validated, anchorAwaitingTestOptions(ctx, t)); !errors.Is(err, retirement.ErrUsed) {
				t.Fatal("completed captured raw operation remained reusable", err)
			}
		})
	}
}

func TestAnchorAwaitingInstallRequiresOriginalCaptureAndStore(t *testing.T) {
	for _, kind := range []string{"uncaptured", "foreign-store", "no-local-provenance", "long-original"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			cursor := source.cursor.Load()
			carrier, authority := cursor.local.raw, anchorAuthority(follower.Authority())
			switch kind {
			case "uncaptured":
				validated := anchorAwaitingTestImage(t.Context(), t, primary, follower)
				_, err := follower.InstallReplica(t.Context(), validated, replication.InstallOptions{ExpectedTerm: primary.Authority().Term})
				if !errors.Is(err, ErrAnchorUnavailable) {
					t.Fatal("raw local carrier admitted implicit capture", err)
				}
			case "foreign-store":
				foreign := mirror(t, primary)
				anchorLocalTestAwaiting(t, foreign)
				if _, err := foreign.BindAnchorLifecycle(t.Context()); err != nil {
					t.Fatal(err)
				}
				ctx := anchorTestInstallContext(t.Context(), t, follower)
				validated := anchorAwaitingTestImage(ctx, t, primary, follower)
				_, err := foreign.InstallReplica(ctx, validated, anchorAwaitingTestOptions(ctx, t))
				if !errors.Is(err, replication.ErrInvalid) || ctx.Value(anchorInstallKey{}).(*anchorInstall).used.Load() {
					t.Fatal("foreign Store consumed original raw operation", err)
				}
			case "no-local-provenance":
				// Deliberate corruption of the actual schema8 carrier is never
				// repaired or treated as a legacy zero-floor/migration preimage.
				result, err := follower.w.ExecContext(t.Context(), `DELETE FROM meta WHERE key=?`, anchorLocalKey)
				if err != nil {
					t.Fatal(err)
				}
				if n, err := result.RowsAffected(); err != nil || n != 1 {
					t.Fatal("fixture did not remove exactly one real carrier", err)
				}
				original, cancel := context.WithTimeout(t.Context(), 10*time.Second)
				defer cancel()
				if _, err := follower.CaptureAnchorInstall(original); !errors.Is(err, ErrAnchorUnavailable) {
					t.Fatal("awaiting raw capture invented missing migration provenance", err)
				}
			case "long-original":
				original, cancel := context.WithTimeout(t.Context(), time.Hour)
				defer cancel()
				if _, err := follower.CaptureAnchorInstall(original); !errors.Is(err, retirement.ErrInvalid) {
					t.Fatal("raw lane widened original acquisition bound", err)
				}
			}
			if kind == "no-local-provenance" {
				if local, err := loadAnchorLocal(t.Context(), follower.r); local != nil || !errors.Is(err, ErrAnchorUnavailable) || source.cursor.Load() != cursor || anchorAuthority(follower.Authority()) != authority {
					t.Fatal("missing durability was repaired or altered admission", err)
				}
			} else {
				anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
			}
		})
	}
}

func TestAnchorAwaitingInstallOriginalCancellationAndDeadline(t *testing.T) {
	for _, kind := range []string{"cancel", "deadline"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			cursor := source.cursor.Load()
			authority, carrier := anchorAuthority(follower.Authority()), cursor.local.raw
			duration := 10 * time.Second
			if kind == "deadline" {
				duration = 2 * time.Second
			}
			original, cancel := context.WithTimeout(t.Context(), duration)
			defer cancel()
			ctx, err := follower.CaptureAnchorInstall(original)
			if err != nil {
				t.Fatal(err)
			}
			validated := anchorAwaitingTestImage(ctx, t, primary, follower)
			if kind == "cancel" {
				cancel()
			} else {
				<-original.Done() // Original timer event, no sleep/recapture/retry.
			}
			_, err = follower.InstallReplica(context.WithoutCancel(ctx), validated, anchorAwaitingTestOptions(ctx, t))
			if !errors.Is(err, original.Err()) {
				t.Fatal("detached wrapper replaced original refusal", err)
			}
			anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
		})
	}
}

func TestAnchorAwaitingInstallNoopRequiresCapturedBaseline(t *testing.T) {
	for _, kind := range []string{"zero", "stale", "successor", "exact"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			mustApply(t, primary, zone("baseline", "baseline.example."), store.ManagerAPI)
			first := anchorTestInstallContext(t.Context(), t, follower)
			initial := anchorAwaitingTestImage(first, t, primary, follower)
			if _, err := follower.InstallReplica(first, initial, anchorAwaitingTestOptions(first, t)); err != nil {
				t.Fatal(err)
			}
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			validated := anchorAwaitingTestImage(ctx, t, primary, follower)
			opts := anchorAwaitingTestOptions(ctx, t)
			switch kind {
			case "zero":
				opts.ExpectedInstalled = replication.Generation{}
			case "stale":
				opts.ExpectedInstalled.Revision--
			case "successor":
				opts.ExpectedInstalled.Revision++
			}
			cursor := source.cursor.Load()
			authority, carrier := anchorAuthority(follower.Authority()), cursor.local.raw
			_, err := follower.InstallReplica(ctx, validated, opts)
			if kind == "exact" {
				if err != nil {
					t.Fatal("exact captured no-op refused", err)
				}
			} else if !errors.Is(err, replication.ErrCAS) {
				t.Fatal("early no-op accepted substituted captured baseline", err)
			}
			anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
		})
	}
}

func TestAnchorAwaitingInstallCannotAdoptInterposedCut(t *testing.T) {
	for _, kind := range []string{"source", "local", "authority", "base"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			first := anchorTestInstallContext(t.Context(), t, follower)
			if _, err := follower.InstallReplica(first, anchorAwaitingTestImage(first, t, primary, follower), anchorAwaitingTestOptions(first, t)); err != nil {
				t.Fatal(err)
			}
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			validated := anchorAwaitingTestImage(ctx, t, primary, follower)
			switch kind {
			case "source":
				// Equal bytes on a distinct published cursor are not the original cut.
				next := *source.cursor.Load()
				source.cursor.Store(&next)
			case "local":
				r := source.cursor.Load().local.record
				r.Carrier = strings.Repeat("f", 32)
				if r.Carrier == source.cursor.Load().local.record.Carrier {
					r.Carrier = strings.Repeat("e", 32)
				}
				next, err := encodeAnchorLocal(r)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := follower.w.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key=?`, next.raw, anchorLocalKey); err != nil {
					t.Fatal(err)
				}
			case "authority":
				a := follower.Authority()
				a.Incarnation++
				tx, err := follower.w.BeginTx(t.Context(), nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = tx.Rollback() }()
				if err := writeAuthority(t.Context(), tx, a); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
			case "base":
				// Deliberate private SQL interposition changes the actual recomputed
				// base without touching authority or the original anchor/local rows.
				if _, err := follower.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime','foreign/interposed','{}')`); err != nil {
					t.Fatal(err)
				}
			}
			before := imageOf(t, follower).Generation()
			cursor := source.cursor.Load()
			carrier := anchorLocalTestRead(t, follower).raw
			authority, err := readAuthority(t.Context(), follower.r)
			if err != nil {
				t.Fatal(err)
			}
			want := replication.ErrCAS
			if kind == "source" || kind == "local" {
				want = accepted.ErrCAS
			}
			_, err = follower.InstallReplica(ctx, validated, anchorAwaitingTestOptions(ctx, t))
			if !errors.Is(err, want) {
				t.Fatal("raw install adopted interposed cut or changed refusal category", err)
			}
			afterAuthority, readErr := readAuthority(t.Context(), follower.r)
			if readErr != nil || anchorAuthority(authority) != anchorAuthority(afterAuthority) {
				t.Fatal("interposition refusal changed durable authority", readErr)
			}
			if !before.Same(imageOf(t, follower).Generation()) || source.cursor.Load() != cursor || anchorLocalTestRead(t, follower).raw != carrier {
				t.Fatal("interposition refusal changed actual current cut")
			}
		})
	}
}

func anchorAwaitingTestInitialize(t *testing.T, primary *Store) accepted.Snapshot {
	t.Helper()
	anchorTestBind(t, primary)
	next, err := accepted.NewDormant(primary.Authority().Term.ClusterID, strings.Repeat("d", 32), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorTestContext(t.Context(), t, primary)
	if err := primary.Command(ctx, primary.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if err := anchorTestStage(ctx, tx, next); err != nil {
			return err
		}
		return tx.InitializeAnchorLocal(ctx)
	}); err != nil {
		t.Fatal(err)
	}
	return next
}

func TestAnchorAwaitingInstallCannotRaceSuccessorBinding(t *testing.T) {
	for _, kind := range []string{"raw", "dormant", "managed"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			original := anchorTestInstallContext(t.Context(), t, follower)
			old := anchorAwaitingTestImage(original, t, primary, follower)
			if kind != "raw" {
				dormant := anchorAwaitingTestInitialize(t, primary)
				if kind == "managed" {
					anchorTestManaged(t, primary, dormant)
				}
			}
			// One independent actual install wins before the older capture is
			// invoked; it is not a retry/rescue of a refused original operation.
			successor := anchorTestInstallContext(t.Context(), t, follower)
			chosen := anchorAwaitingTestImage(successor, t, primary, follower)
			if _, err := follower.InstallReplica(successor, chosen, anchorAwaitingTestOptions(successor, t)); err != nil {
				t.Fatal(err)
			}
			cursor := source.cursor.Load()
			before := imageOf(t, follower).Generation()
			_, err := follower.InstallReplica(original, old, anchorAwaitingTestOptions(original, t))
			want := accepted.ErrCAS
			if kind == "raw" {
				want = replication.ErrCAS
			}
			if !errors.Is(err, want) {
				t.Fatal("old awaiting capture admitted successor source/baseline or changed refusal category", err)
			}
			if source.cursor.Load() != cursor || !before.Same(imageOf(t, follower).Generation()) {
				t.Fatal("old raw capture displaced actual successor")
			}
			if kind == "raw" {
				anchorAwaitingTestNoGrant(t, source, cursor)
			} else if cursor.awaiting || cursor.gate == nil {
				t.Fatal("independent binding fixture never installed actual history")
			}
		})
	}
}

func TestAnchorAwaitingInstallConcurrentFirstCapturesCAS(t *testing.T) {
	primary, follower, source := anchorAwaitingTestPair(t)
	cursor := source.cursor.Load()
	left := anchorTestInstallContext(t.Context(), t, follower)
	right := anchorTestInstallContext(t.Context(), t, follower)
	validated := anchorAwaitingTestImage(left, t, primary, follower)
	start := make(chan struct{})
	results := make(chan error, 2)
	for _, ctx := range []context.Context{left, right} {
		opts := anchorAwaitingTestOptions(ctx, t)
		go func() {
			<-start
			_, err := follower.InstallReplica(ctx, validated, opts)
			results <- err
		}()
	}
	close(start)
	passed, refused := 0, 0
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				passed++
			} else if errors.Is(err, replication.ErrCAS) {
				refused++
			} else {
				t.Fatal("unexpected concurrent first-install refusal", err)
			}
		case <-left.Done():
			t.Fatal("original bounded concurrent fixtures did not join")
		}
	}
	if passed != 1 || refused != 1 {
		t.Fatal("two original zero baselines both installed or neither installed")
	}
	anchorAwaitingTestNoGrant(t, source, cursor)
}

func TestAnchorAwaitingInstallSQLBoundaryAndPostCommitCancel(t *testing.T) {
	for _, kind := range []string{"rollback", "commit-unknown", "postcommit-cancel", "noop"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t)
			if kind == "noop" {
				first := anchorTestInstallContext(t.Context(), t, follower)
				if _, err := follower.InstallReplica(first, anchorAwaitingTestImage(first, t, primary, follower), anchorAwaitingTestOptions(first, t)); err != nil {
					t.Fatal(err)
				}
			}
			cursor := source.cursor.Load()
			authority, carrier := anchorAuthority(follower.Authority()), cursor.local.raw
			switch kind {
			case "rollback":
				if _, err := follower.w.ExecContext(t.Context(), `CREATE TEMP TRIGGER anchor_raw_abort BEFORE INSERT ON main.replication_sections BEGIN SELECT RAISE(ABORT,'fixture raw install refused'); END`); err != nil {
					t.Fatal(err)
				}
			case "commit-unknown":
				for _, statement := range []string{
					`CREATE TEMP TABLE anchor_raw_parent(id INTEGER PRIMARY KEY)`,
					`CREATE TEMP TABLE anchor_raw_child(parent INTEGER REFERENCES anchor_raw_parent(id) DEFERRABLE INITIALLY DEFERRED)`,
					`CREATE TEMP TRIGGER anchor_raw_unknown AFTER INSERT ON main.replication_sections BEGIN INSERT INTO anchor_raw_child(parent) VALUES (1); END`,
				} {
					if _, err := follower.w.ExecContext(t.Context(), statement); err != nil {
						t.Fatal(err)
					}
				}
			}
			original, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			ctx, err := follower.CaptureAnchorInstall(original)
			if err != nil {
				t.Fatal(err)
			}
			validated := anchorAwaitingTestImage(ctx, t, primary, follower)
			image := validated.Image()
			if _, err := follower.ValidateImage(ctx, image); err != nil {
				t.Fatal("receiver revalidation failed before injected boundary", err)
			}
			opts := anchorAwaitingTestOptions(ctx, t)
			var operation *anchorOperation
			// Exact public dispatcher/body sequence, retaining only private
			// disposition inspection; this case does not claim entry-wrapper
			// coverage. The other raw cases call public InstallReplica directly.
			err = follower.withAnchorInstall(ctx, image, func(ctx context.Context, current *anchorOperation) error {
				operation = current
				_, result := follower.installReplica(ctx, image, opts, current)
				if kind == "postcommit-cancel" && current.sql.outcome == AnchorCommitted {
					cancel()
				}
				return result
			})
			if operation == nil || !operation.awaitingInstall || operation.scope != nil || operation.change != nil || operation.installGate != nil || operation.sql.tx == nil || operation.localTx != operation.sql.tx {
				t.Fatal("fixture did not reach captured raw SQL lane")
			}
			if _, ok := operation.receipt().Version(); ok {
				t.Fatal("raw completion invented accepted Version receipt")
			}
			switch kind {
			case "noop":
				if err != nil || operation.sql.attempted || operation.sql.committed || operation.sql.outcome != AnchorUnchanged || !operation.prepared || operation.target != cursor {
					t.Fatal("raw no-op did not retain actual unchanged disposition", err)
				}
				anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
			case "postcommit-cancel":
				if err != nil || !operation.sql.attempted || !operation.sql.committed || operation.sql.outcome != AnchorCommitted || original.Err() != context.Canceled {
					t.Fatal("actual raw commit was reclassified after cancellation", err)
				}
				if !image.Generation().Same(imageOf(t, follower).Generation()) || source.closed.Load() {
					t.Fatal("post-effect cancellation lost committed raw effect")
				}
			default:
				var sqlFailure interface{ Code() int }
				if !errors.As(err, &sqlFailure) {
					t.Fatal("fixture failed before actual SQL target", err)
				}
				if kind == "rollback" {
					if sqlFailure.Code() != 1811 || operation.sql.attempted || operation.sql.committed || operation.sql.outcome != AnchorRolledBack || errors.Is(err, sql.ErrTxDone) {
						t.Fatal("SQL trigger did not confirm actual precommit rollback")
					}
				} else if sqlFailure.Code() != 787 || !operation.sql.attempted || operation.sql.committed || operation.sql.outcome != AnchorUnknown || !errors.Is(err, sql.ErrTxDone) || !source.closed.Load() || !follower.anchorBlocked.Load() {
					t.Fatal("actual unknown commit reopened/credited raw admission")
				}
				anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
			}
			anchorAwaitingTestNoGrant(t, source, cursor)
		})
	}
}

func anchorAwaitingTestResolved(t *testing.T) (*Store, *Store, *AnchorSource, *replication.Validated, replication.ResolveRequest, replication.Authority) {
	t.Helper()
	primary, follower, source := anchorAwaitingTestPair(t)
	initialCtx := anchorTestInstallContext(t.Context(), t, follower)
	initial := anchorAwaitingTestImage(initialCtx, t, primary, follower)
	if _, err := follower.InstallReplica(initialCtx, initial, anchorAwaitingTestOptions(initialCtx, t)); err != nil {
		t.Fatal(err)
	}
	divergeCtx := anchorTestInstallContext(t.Context(), t, follower)
	chosenImage := anchorAwaitingTestImage(divergeCtx, t, primary, follower).Image()
	chosenImage.Payload.Runtime = append(slices.Clone(chosenImage.Payload.Runtime), replication.Entry{Key: "foreign/chosen", Data: json.RawMessage(`{"chosen":true}`)})
	if err := replication.Seal(&chosenImage); err != nil {
		t.Fatal(err)
	}
	chosen, err := follower.ValidateImage(divergeCtx, chosenImage)
	if err != nil {
		t.Fatal(err)
	}
	_, err = follower.InstallReplica(divergeCtx, chosen, anchorAwaitingTestOptions(divergeCtx, t))
	var completed *AnchorCompletionError
	if !errors.Is(err, replication.ErrDivergent) || !errors.As(err, &completed) || completed.Completion().Outcome() != AnchorCommitted {
		t.Fatal("metadata-only raw divergence lost actual commit disposition", err)
	}
	if _, ok := completed.Completion().Version(); ok {
		t.Fatal("raw divergence minted owner Version")
	}
	base := initial.Image().Generation()
	if _, err := follower.ReadQuarantined(t.Context(), base); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("metadata-only divergence quarantined unchanged canonical branch", err)
	}
	resolveCtx := anchorTestInstallContext(t.Context(), t, follower)
	// Select the already immutable conflicting branch after original capture.
	req := replication.ResolveRequest{ExpectedTerm: follower.Authority().Term, ExpectedInstalled: resolveCtx.Value(anchorInstallKey{}).(*anchorInstall).installed, ExpectedDivergence: follower.Authority().Divergence, RequestID: "raw-original-resolution"}
	resolved, err := follower.ResolveConflict(resolveCtx, chosen, req)
	if err != nil || resolved.Mode != replication.Primary || resolved.Parent == nil || !resolved.Parent.Same(chosenImage.Generation()) {
		t.Fatal("captured raw resolution lost selected branch", err)
	}
	quarantined, err := follower.ReadQuarantined(t.Context(), base)
	if err != nil || !quarantined.Generation().Same(base) {
		t.Fatal("raw resolution lost exact quarantine", err)
	}
	return primary, follower, source, chosen, req, resolved
}

func TestAnchorAwaitingResolveRequestNoopRequiresCapturedBaseline(t *testing.T) {
	for _, kind := range []string{"zero", "stale", "successor", "exact", "uncaptured", "actual-base-interposed"} {
		t.Run(kind, func(t *testing.T) {
			_, follower, source, chosen, req, resolved := anchorAwaitingTestResolved(t)
			cursor := source.cursor.Load()
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			req.ExpectedInstalled = ctx.Value(anchorInstallKey{}).(*anchorInstall).installed
			switch kind {
			case "zero":
				req.ExpectedInstalled = replication.Generation{}
			case "stale":
				req.ExpectedInstalled = chosen.Image().Generation()
			case "successor":
				req.ExpectedInstalled.Revision++
			case "actual-base-interposed":
				if _, err := follower.w.ExecContext(t.Context(), `INSERT INTO replication_sections(section,key,data) VALUES ('runtime','foreign/interposed','{}')`); err != nil {
					t.Fatal(err)
				}
			case "uncaptured":
				ctx = t.Context()
			}
			authority, carrier := anchorAuthority(follower.Authority()), cursor.local.raw
			before := imageOf(t, follower).Generation()
			after, err := follower.ResolveConflict(ctx, chosen, req)
			if kind == "exact" {
				if err != nil || !after.Term.Same(resolved.Term) {
					t.Fatal("exact captured RequestID no-op refused", err)
				}
			} else if kind == "uncaptured" {
				if !errors.Is(err, ErrAnchorUnavailable) {
					t.Fatal("raw RequestID replay silently captured authority", err)
				}
			} else if !errors.Is(err, replication.ErrCAS) {
				t.Fatal("RequestID early return accepted substituted original baseline", err)
			}
			anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
			anchorAwaitingTestNoGrant(t, source, cursor)
			if !before.Same(imageOf(t, follower).Generation()) {
				t.Fatal("RequestID no-op/refusal changed canonical rows")
			}
		})
	}
}

func TestAnchorAwaitingInstallReservedPrefixAndCompleteValidators(t *testing.T) {
	for _, kind := range []string{"unknown-owned-version", "extra-owned-key", "missing-sentinel", "complete-image-validator", "complete-entry-validator"} {
		t.Run(kind, func(t *testing.T) {
			primary, follower, source := anchorAwaitingTestPair(t, func(o *Options) {
				if kind == "complete-image-validator" {
					o.ValidateReplicaImage = accepted.ValidateImage
				}
				if kind == "complete-entry-validator" {
					o.ValidateReplicaEntry = accepted.ValidateEntry
				}
			})
			ctx := anchorTestInstallContext(t.Context(), t, follower)
			original, err := primary.ReadGeneration(ctx)
			if err != nil {
				t.Fatal(err)
			}
			image := *original
			switch kind {
			case "unknown-owned-version":
				image.Payload.Runtime = append(slices.Clone(image.Payload.Runtime), replication.Entry{Key: "trustanchors/v99/root", Data: json.RawMessage(`{}`)})
			case "extra-owned-key":
				image.Payload.Runtime = append(slices.Clone(image.Payload.Runtime), replication.Entry{Key: accepted.ChunkPrefix + strings.Repeat("a", 64), Data: json.RawMessage(`{}`)})
			case "missing-sentinel":
				image.Payload.Runtime = nil
			}
			if err := replication.Seal(&image); err != nil {
				t.Fatal(err)
			}
			// Validated transport evidence deliberately comes from a raw sender;
			// InstallReplica must still invoke the receiver's complete validators.
			validated, err := primary.ValidateImage(ctx, image)
			if err != nil {
				t.Fatal("malformed-owner fixture failed at transport validation", err)
			}
			cursor := source.cursor.Load()
			authority, carrier := anchorAuthority(follower.Authority()), cursor.local.raw
			_, err = follower.InstallReplica(ctx, validated, anchorAwaitingTestOptions(ctx, t))
			want := accepted.ErrInvalid
			if kind == "missing-sentinel" {
				want = accepted.ErrMissing
			}
			if !errors.Is(err, want) {
				t.Fatal("raw lane bypassed owner prefix/complete validation", err)
			}
			anchorAwaitingTestUnchanged(t, follower, source, cursor, authority, carrier)
			anchorAwaitingTestNoGrant(t, source, cursor)
		})
	}
}
