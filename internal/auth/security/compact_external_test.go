package security

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

func TestCheckpointRejectsOldOIDCProofPreservesUnrelatedEnrollment(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	at := f.now.Add(time.Second)
	proof := f.proof(t, at, "alice", "ops")
	enrollment := f.issue(t, proof, at)
	cookie, _, err := enrollment.Seal(f.view(t), at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	credential := servingauth.Credential{Kind: servingauth.OIDCJWT, Value: proof.RawIDToken}
	principal, err := f.view(t).Authenticate(t.Context(), credential, at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	state := ownerState(t, f.st)
	floor := at.Add(2 * time.Second)
	fillGrantHistory(&state, compactHistoryRows, floor)
	seedHistory(t, f.st, state)
	before, _ := f.st.ReadGeneration(t.Context())
	// Resolution precedes compaction in issuance. Its original proof must be
	// rechecked after compaction, rather than acquiring the new enrollment time.
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueExternal(ctx, tx, f.deps, proof, func() time.Time { return floor.Add(time.Second) })
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("pre-compaction proof crossed checkpoint during issuance", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("rejected issuance committed checkpoint or identity changes")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	check := func() {
		t.Helper()
		v := f.view(t)
		now := floor.Add(time.Second)
		if _, err := v.Authenticate(t.Context(), credential, now); !errors.Is(err, servingauth.ErrCredentials) {
			t.Fatal("checkpoint revived old raw JWT", err)
		}
		if _, err := v.LoadPrincipal(principal, now); !errors.Is(err, servingauth.ErrCredentials) {
			t.Fatal("checkpoint trusted pre-checkpoint JWT evidence", err)
		}
		if _, err := v.CookieSession(cookie, now); err != nil {
			t.Fatal("checkpoint logged out unrelated enrolled OIDC session", err)
		}
	}
	check()
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	check()
	f.st = promotedCheckpointReplica(t, f.st)
	check()
	freshAt := floor.Add(3 * time.Second)
	fresh := f.proof(t, freshAt, "alice", "ops")
	if _, err := f.view(t).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.OIDCJWT, Value: fresh.RawIDToken}, freshAt); err != nil {
		t.Fatal("checkpoint prevented fresh OIDC authentication", err)
	}
	f.issue(t, fresh, freshAt)
}

func TestCheckpointKeepsProviderFloorForOlderEnrolledEvidence(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	at := f.now.Add(time.Second)
	proof := f.proof(t, at, "alice", "ops")
	enrollment := f.issue(t, proof, at.Add(4*time.Second))
	cookie, _, err := enrollment.Seal(f.view(t), at.Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	state := ownerState(t, f.st)
	// The enrollment timestamp is newer than this floor, but the proof inside
	// it is older. Compaction must not excuse that proof using enrollment time.
	state.ProviderFloors = []servingauth.ProviderFloor{{FormatVersion: 1, Provider: "corp", UID: string(f.provider.UID), ValidAfter: at.Add(2 * time.Second)}}
	fillGrantHistory(&state, compactHistoryRows, at.Add(3*time.Second))
	seedHistory(t, f.st, state)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	if _, err := f.view(t).CookieSession(cookie, at.Add(6*time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("compaction revived revoked provider evidence", err)
	}
	if len(ownerState(t, f.st).ProviderFloors) != 1 {
		t.Fatal("compaction removed a floor still used by enrolled evidence")
	}
}

func TestCompactedGrantRevocationSurvivesRegrant(t *testing.T) {
	f := newSessionFixture(t)
	cookie, err := f.view(t).SealSession(f.row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	// Remove and recreate the real binding, then force the resulting grant
	// revocation to compact. The old cookie must remain denied after regrant.
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return Delete(ctx, tx, f.binding, store.DeleteOptions{Manager: store.ManagerAPI}, f.now)
	})
	state := ownerState(t, f.st)
	fillGrantHistory(&state, compactHistoryRows-1, f.now)
	seedHistory(t, f.st, state)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := Apply(ctx, tx, f.binding, store.ApplyOptions{Manager: store.ManagerAPI}, f.now.Add(time.Second))
		return err
	})
	if _, err := f.view(t).CookieSession(cookie, f.now.Add(2*time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("regrant revived compacted revoked cookie", err)
	}
	if len(ownerState(t, f.st).GrantFloors) != 0 {
		t.Fatal("test failed to compact the revoked grant")
	}
}

func TestByteCompactionRechecksOriginalOIDCProofAfterCommitStaging(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	at := f.now.Add(time.Second)
	proof := f.proof(t, at, "alice", "ops")
	f.issue(t, proof, at)
	state := ownerState(t, f.st)
	floor := at.Add(2 * time.Second)
	// Below both the history-count trigger and row bound: adding the session
	// must reach the aggregate byte-bound fallback inside State.Commit.
	fillGrantHistory(&state, 24, floor)
	seedHistory(t, f.st, state)
	padSecurityTo(t, f.st, servingauth.MaxSecurityBytes-256)
	before, _ := f.st.ReadGeneration(t.Context())
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueExternal(ctx, tx, f.deps, proof, func() time.Time { return floor.Add(time.Second) })
		return err
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("byte compaction excused stale JWT using new enrollment time", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("byte compaction committed after original proof refusal")
	}
}
