package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/retirement"
)

// This fixture executes the actual unmounted migration helper + marker in one
// SQLite transaction. It does NOT claim the parent's schema8 registration,
// runtime feature, startup callers or old full-suite conversion are mounted.
func anchorLocalTestPhysical(t *testing.T, s *Store) {
	t.Helper()
	tx, err := s.w.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := migrateAnchorLocal(t.Context(), tx); err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(t.Context(), `UPDATE meta SET value='8' WHERE key=? AND value='7'`, metaSchemaVersion)
	if err != nil {
		t.Fatal(err)
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		t.Fatal("fixture did not advance exact marker", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func anchorLocalTestDormant(t *testing.T, s *Store) (*AnchorSource, accepted.Snapshot) {
	t.Helper()
	anchorLocalTestPhysical(t, s)
	source := anchorTestBind(t, s)
	dormant, err := accepted.NewDormant(s.Authority().Term.ClusterID, strings.Repeat("c", 32), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	ctx := anchorTestContext(t.Context(), t, s)
	err = s.Command(ctx, s.Authority().Term, func(ctx context.Context, tx *CommandTx) error {
		if err := anchorTestStage(ctx, tx, dormant); err != nil {
			return err
		}
		return tx.InitializeAnchorLocal(ctx)
	})
	if err != nil {
		t.Fatal(err)
	}
	return source, dormant
}

func anchorLocalTestManaged(t *testing.T) (*Store, *AnchorSource, anchorTestProof) {
	t.Helper()
	s := openTemp(t, internalOptions)
	source, dormant := anchorLocalTestDormant(t, s)
	proof := anchorTestManaged(t, s, dormant)
	return s, source, proof
}

func anchorLocalTestRead(t *testing.T, s *Store) *anchorLocal {
	t.Helper()
	value, err := loadAnchorLocal(t.Context(), s.r)
	if err != nil || value == nil {
		t.Fatal("missing fixture local carrier", err)
	}
	return value
}

func TestAnchorLocalCodecExactPrivateShape(t *testing.T) {
	s, source, _ := anchorLocalTestManaged(t)
	value := anchorLocalTestRead(t, s)
	if !sameAnchorLocal(value, source.cursor.Load().local) {
		t.Fatal("committed local/source split")
	}
	decoded, err := decodeAnchorLocal(value.raw)
	if err != nil || !sameAnchorLocal(value, decoded) {
		t.Fatal("canonical roundtrip", err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal([]byte(value.raw), &object); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"unknown", "duplicate", "null-denied", "omitted", "trailing", "space", "invalid-utf8", "alternate-number", "surrogate"} {
		t.Run(kind, func(t *testing.T) {
			raw := value.raw
			switch kind {
			case "unknown":
				raw = strings.TrimSuffix(raw, "}") + `,"unknown":true}`
			case "duplicate":
				raw = strings.TrimSuffix(raw, "}") + `,"format":1}`
			case "null-denied":
				raw = strings.Replace(raw, `"denied":[]`, `"denied":null`, 1)
			case "omitted":
				raw = strings.Replace(raw, `"terminal":false`, `"terminalOmitted":false`, 1)
			case "trailing":
				raw += "{}"
			case "space":
				raw = " " + raw
			case "invalid-utf8":
				raw = string([]byte{0xff})
			case "alternate-number":
				raw = strings.Replace(raw, `"revision":"3"`, `"revision":"03"`, 1)
			case "surrogate":
				raw = strings.Replace(raw, `"member":`, `"member":"\ud800","ignored":`, 1)
			}
			if _, err := decodeAnchorLocal(raw); err == nil {
				t.Fatal("noncanonical carrier accepted")
			}
		})
	}
	if _, err := decodeAnchorLocal(strings.Repeat("x", anchorLocalMaxBytes+1)); !errors.Is(err, accepted.ErrBounds) {
		t.Fatal("unbounded record", err)
	}
}

func TestAnchorLocalFixedHistoryMemberDomainAndOwner(t *testing.T) {
	_, source, _ := anchorLocalTestManaged(t)
	old := source.cursor.Load()
	for _, kind := range []string{"member", "history", "domain", "owner"} {
		t.Run(kind, func(t *testing.T) {
			record := old.local.record
			switch kind {
			case "member":
				record.Member = "different-member"
			case "history":
				record.History = strings.Repeat("d", 32)
			case "domain":
				record.Domain = strings.Repeat("e", 64)
			case "owner":
				record.Owner.Digest = strings.Repeat("f", 64)
			}
			changed, err := encodeAnchorLocal(record)
			if err != nil {
				t.Fatal(err)
			}
			if err := checkAnchorLocal(changed, old, old.local.record.Member); err == nil {
				t.Fatal("mixed carrier accepted")
			}
		})
	}
}

func TestAnchorLocalBoundedPermanentUnionAndSaturation(t *testing.T) {
	_, source, _ := anchorLocalTestManaged(t)
	current := source.cursor.Load()
	ids := make([]string, anchorLocalMaxRevocations)
	const digits = "0123456789abcdef"
	for i := range ids {
		ids[i] = strings.Repeat("0", 62) + string([]byte{digits[i/16], digits[i%16]})
	}
	full, err := anchorLocalDeny(current.local, current, ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.record.Denied) != 256 || full.record.Terminal {
		t.Fatal("full retained union changed")
	}
	same, err := anchorLocalDeny(full, current, ids)
	if err != nil || !sameAnchorLocal(same, full) {
		t.Fatal("no-op advanced local identity", err)
	}
	saturated, err := anchorLocalDeny(full, current, []string{strings.Repeat("f", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if !saturated.record.Terminal || saturated.record.RetiredThrough != math.MaxUint64 || len(saturated.record.Denied) != 256 {
		t.Fatal("capacity evicted/wrapped denial")
	}
	for i, d := range full.record.Denied {
		if saturated.record.Denied[i] != d {
			t.Fatal("first receipt changed")
		}
	}
	record := current.local.record
	record.Revision = math.MaxUint64 - 1
	near, err := encodeAnchorLocal(record)
	if err != nil {
		t.Fatal(err)
	}
	terminal, err := anchorLocalDeny(near, current, []string{ids[0]})
	if err != nil {
		t.Fatal(err)
	}
	if terminal.record.Revision != math.MaxUint64 || !terminal.record.Terminal {
		t.Fatal("revision exhaustion wrapped")
	}
	if _, err := advanceAnchorLocal(terminal, terminal.record); !errors.Is(err, ErrAnchorUnavailable) {
		t.Fatal("terminal record rearmed", err)
	}
}

func TestAnchorLocalUnknownNamespaceAndCurrentSchemaMissingRefuse(t *testing.T) {
	for _, kind := range []string{"missing", "unknown", "extra", "oversized"} {
		t.Run(kind, func(t *testing.T) {
			s := openTemp(t)
			anchorLocalTestPhysical(t, s)
			switch kind {
			case "missing":
				if _, err := s.w.ExecContext(t.Context(), `DELETE FROM meta WHERE key=?`, anchorLocalKey); err != nil {
					t.Fatal(err)
				}
			case "unknown":
				if _, err := s.w.ExecContext(t.Context(), `UPDATE meta SET key='trustanchors-local/v99/state' WHERE key=?`, anchorLocalKey); err != nil {
					t.Fatal(err)
				}
			case "extra":
				if _, err := s.w.ExecContext(t.Context(), `INSERT INTO meta(key,value) VALUES ('trustanchors-local/orphan','{}')`); err != nil {
					t.Fatal(err)
				}
			case "oversized":
				if _, err := s.w.ExecContext(t.Context(), `UPDATE meta SET value=? WHERE key=?`, strings.Repeat("x", anchorLocalMaxBytes+1), anchorLocalKey); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.BindAnchorLifecycle(anchorTestContext(t.Context(), t, s)); err == nil {
				t.Fatal("invalid private namespace bound lifecycle")
			}
			if s.anchors.Load() != nil {
				t.Fatal("invalid carrier registered")
			}
		})
	}
}

func TestAnchorLocalCASNeverUpsertsOrAdoptsSuccessor(t *testing.T) {
	s, _, _ := anchorLocalTestManaged(t)
	old := anchorLocalTestRead(t, s)
	nextRecord := old.record
	nextRecord.Revision++
	next, err := encodeAnchorLocal(nextRecord)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.w.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := casAnchorLocal(t.Context(), tx, old, next); err != nil {
		t.Fatal(err)
	}
	// Reusing the old raw value refuses; it must not update the new local version.
	newerRecord := next.record
	newerRecord.Revision++
	newer, err := encodeAnchorLocal(newerRecord)
	if err != nil {
		t.Fatal(err)
	}
	if err := casAnchorLocal(t.Context(), tx, old, newer); !errors.Is(err, accepted.ErrCAS) {
		t.Fatal("local CAS adopted successor", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !sameAnchorLocal(old, anchorLocalTestRead(t, s)) {
		t.Fatal("rollback lost original carrier")
	}
}

func TestAnchorLocalStaticSchema7CompatibilityAndNakedManagedRefusal(t *testing.T) {
	s := openTemp(t)
	source := anchorTestBind(t, s)
	dormant := anchorTestDormant(t, s)
	if source.cursor.Load().local != nil {
		t.Fatal("schema7 invented local provenance")
	}
	update, err := s.CaptureAnchorUpdate(anchorTestContext(t.Context(), t, s), dormant.Version())
	if err != nil {
		t.Fatal(err)
	}
	proof := anchorTestBootstrap(t, s, dormant)
	result, err := s.AnchorCommand(update, func(ctx context.Context, tx *CommandTx) error { return anchorTestStage(ctx, tx, proof.next) })
	if !errors.Is(err, ErrAnchorUnavailable) || result.Outcome() != AnchorRolledBack {
		t.Fatal("naked managed schema7 inferred deny zero", err)
	}
	if source.cursor.Load().owner.Version() != dormant.Version() {
		t.Fatal("failed bootstrap replaced source")
	}
	if _, err := s.CaptureAnchorInstall(t.Context()); !errors.Is(err, retirement.ErrInvalid) {
		t.Fatal("unbounded original accepted", err)
	}
}
