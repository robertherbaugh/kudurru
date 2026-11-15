package security

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func seedHistory(t *testing.T, st *sqlite.Store, state servingauth.Security) {
	t.Helper()
	rows, err := servingauth.EncodeSecurity(state)
	if err != nil {
		t.Fatal(err)
	}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, row := range rows {
			if err := tx.PutSection(ctx, "security", row); err != nil {
				return err
			}
		}
		return nil
	})
}

func fillGrantHistory(state *servingauth.Security, count int, at time.Time) {
	for n := range count {
		state.GrantFloors = append(state.GrantFloors, servingauth.GrantFloor{FormatVersion: 1, Grant: fmt.Sprintf("viewer@zone:retired-%d", n), ValidAfter: at})
	}
}

func padSecurityTo(t *testing.T, st *sqlite.Store, size int) {
	t.Helper()
	image, err := st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	remaining := size
	for _, row := range image.Payload.Security {
		remaining -= len(row.Data)
	}
	command(t, st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for remaining > 0 {
			size := min(remaining, servingauth.MaxRowBytes)
			if remaining-size == 1 {
				size--
			}
			payload, _ := json.Marshal(strings.Repeat("x", size-2))
			if err := tx.PutSection(ctx, "security", replication.Entry{Key: fmt.Sprintf("other-owner/padding-%d", remaining), Data: payload}); err != nil {
				return err
			}
			remaining -= len(payload)
		}
		return nil
	})
}

func TestIAMChurnCrossesFormerHistoryCapacity(t *testing.T) {
	f := newSessionFixture(t)
	deps := loginDependencies(t, f)
	// Keep one unrelated enrolled session across all checkpoints.
	var stable Enrollment
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		stable, err = IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}, f.now.Add(time.Second))
		return err
	})
	stableCookie, _, err := stable.Seal(f.view(t), f.now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	var oldest servingauth.SessionRecord
	var oldestCookie string
	for n := range servingauth.MaxRows + 17 {
		at := f.now.Add(2*time.Second + time.Duration(n)*time.Millisecond)
		command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
			var template store.Object = &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "churn", CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}}
			provider := "local"
			if n%2 == 1 {
				template = &publicv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: "churn", Namespace: "default", CreationTimestamp: metav1.NewTime(f.now.Add(-time.Hour))}}
				provider = "kudurru"
			}
			created, err := Apply(ctx, tx, template, store.ApplyOptions{Manager: store.ManagerAPI}, at)
			if err != nil {
				return err
			}
			if n > 0 && string(created.GetUID()) == oldest.Account.UID {
				return errors.New("recreated account reused original UID")
			}
			// Enroll a representative credential before each compaction window;
			// the history limit itself is crossed by real account deletions.
			if n%(compactHistoryRows/2) == 0 {
				s, err := Load(ctx, tx)
				if err != nil {
					return err
				}
				r, err := s.Enroll(servingauth.SessionRecord{ID: randomID(), Account: accountRef(created), Provider: provider, IssuedAt: at, ExpiresAt: at.Add(time.Hour), IdleExpiresAt: at.Add(time.Hour), Grants: []string{"viewer"}, CSRFHash: servingauth.CSRFHash(f.csrf)})
				if err != nil {
					return err
				}
				if err := s.Commit(ctx); err != nil {
					return err
				}
				if n == 0 {
					oldest = r
					return nil // Publish and seal the first credential before deletion.
				}
			}
			return Delete(ctx, tx, created, store.DeleteOptions{Manager: store.ManagerAPI}, at.Add(time.Microsecond))
		})
		if n == 0 {
			oldestCookie, err = f.view(t).SealSession(oldest.ID, f.csrf)
			if err != nil {
				t.Fatal(err)
			}
			command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
				return Delete(ctx, tx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: "churn"}}, store.DeleteOptions{Manager: store.ManagerAPI}, at.Add(time.Microsecond))
			})
		}
	}
	state := ownerState(t, f.st)
	if state.RevocationCheckpoint.IsZero() || len(state.AccountFloors) >= compactHistoryRows || len(state.Sessions) > compactHistoryRows+1 {
		t.Fatal("account churn retained permanent history")
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	f.st = openSecurityStore(t, f.path)
	at := f.now.Add(time.Minute)
	if _, err := f.view(t).CookieSession(oldestCookie, at); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("restart/name reuse revived retired cookie", err)
	}
	if _, err := f.view(t).CookieSession(stableCookie, at); err != nil {
		t.Fatal("checkpoint invalidated unrelated local session", err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		_, err := IssueLocal(ctx, tx, deps, servingauth.Credential{Kind: servingauth.Password, Username: "alice", Value: "test-local-password-1"}, at)
		return err
	})
}

func TestFullHistoryCompactionRollbackRestartAndPromotion(t *testing.T) {
	f := newSessionFixture(t)
	state := ownerState(t, f.st)
	fillGrantHistory(&state, servingauth.MaxRows-1-len(state.Sessions), f.now)
	seedHistory(t, f.st, state)
	before, _ := f.st.ReadGeneration(t.Context())
	abort := errors.New("post-compaction failure")
	mutate := func(ctx context.Context, tx *sqlite.CommandTx) error {
		return Delete(ctx, tx, f.user, store.DeleteOptions{Manager: store.ManagerAPI}, f.now.Add(time.Second))
	}
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := mutate(ctx, tx); err != nil {
			return err
		}
		return abort
	})
	if !errors.Is(err, abort) {
		t.Fatal("full history prevented recoverable delete", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("failed delete committed compaction independently")
	}
	command(t, f.st, mutate)
	if got := ownerState(t, f.st); !got.RevocationCheckpoint.Equal(f.now) || len(got.GrantFloors) != 0 {
		t.Fatal("full history did not compact")
	}
	peer := promotedCheckpointReplica(t, f.st)
	if !ownerState(t, peer).RevocationCheckpoint.Equal(f.now) {
		t.Fatal("restart/promotion lost revocation checkpoint")
	}
}

func promotedCheckpointReplica(t *testing.T, source *sqlite.Store) *sqlite.Store {
	t.Helper()
	scheme, _ := runtimekinds.NewScheme()
	private := []store.Kind{}
	for _, kind := range runtimekinds.Private() {
		private = append(private, store.Kind(kind))
	}
	path := filepath.Join(t.TempDir(), "peer.db")
	open := func() *sqlite.Store {
		t.Helper()
		peer, err := sqlite.Open(path, sqlite.Options{Scheme: scheme, InternalKinds: private, InitialMode: replication.Follower, ValidateReplicaEntry: servingauth.ValidateEntry, ValidateReplicaImage: func(i *replication.Image) error { _, err := servingauth.DecodeSecurity(i.Payload.Security); return err }})
		if err != nil {
			t.Fatal(err)
		}
		return peer
	}
	peer := open()
	if err := peer.Enroll(t.Context(), source.Authority().Term); err != nil {
		t.Fatal(err)
	}
	ctx := captureSchema8Install(t.Context(), t, peer)
	image, err := source.ReadGeneration(ctx)
	if err != nil {
		t.Fatal(err)
	}
	validated, err := peer.ValidateImage(ctx, *image)
	if err != nil {
		t.Fatal(err)
	}
	authority := peer.Authority()
	installed, err := peer.InstallReplica(ctx, validated, replication.InstallOptions{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed})
	if err != nil {
		t.Fatal(err)
	}
	_ = peer.Close()
	peer = open()
	t.Cleanup(func() { _ = peer.Close() })
	if _, err := peer.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "checkpoint-promotion"}); err != nil {
		t.Fatal(err)
	}
	return peer
}

func TestCompactionPreservesUnaffectedTokensAndFactorReplay(t *testing.T) {
	f := newSessionFixture(t)
	var keepWire, revokedID string
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, grant := range []string{"viewer", "viewer@zone:retired-0"} {
			wire, id := tokens.Generate()
			_, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(f.now.Add(-time.Minute))}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(wire), Owner: internalv1.TokenOwner{Kind: internalv1.OwnerUser, ID: "local:alice", Provider: "local", Name: "alice", UID: string(f.user.UID)}, Grants: []string{grant}}}, store.ApplyOptions{Manager: store.ManagerSystem})
			if err != nil {
				return err
			}
			if grant == "viewer" {
				keepWire = wire
			} else {
				revokedID = id
			}
		}
		return nil
	})
	state := ownerState(t, f.st)
	state.TOTP = []totp.Record{
		{FormatVersion: 1, Account: totp.Account{Name: "alice", UID: string(f.user.UID)}, SeedVersion: 3, SeedID: "00000000000000000000000000000001", HasConsumed: true, LastConsumedStep: 12345},
		{FormatVersion: 1, Account: totp.Account{Name: "alice", UID: "deleted-incarnation"}, SeedVersion: 1, SeedID: "00000000000000000000000000000002", HasConsumed: true, LastConsumedStep: 12344},
	}
	fillGrantHistory(&state, compactHistoryRows, f.now)
	seedHistory(t, f.st, state)
	before, _ := f.st.ReadGeneration(t.Context())
	abort := errors.New("final admission rejected compaction")
	err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if err := s.Commit(ctx); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return abort })
	})
	if !errors.Is(err, abort) {
		t.Fatal("compaction did not reach final admission", err)
	}
	after, _ := f.st.ReadGeneration(t.Context())
	if !before.Generation().Same(after.Generation()) {
		t.Fatal("denied final admission committed token deletion or replay cleanup")
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	if _, err := f.view(t).Authenticate(t.Context(), servingauth.Credential{Kind: servingauth.APIToken, Value: keepWire}, f.now.Add(time.Second)); err != nil {
		t.Fatal("checkpoint revoked unrelated API token", err)
	}
	var token internalv1.APIToken
	if err := f.st.Get(t.Context(), store.Key{Kind: "APIToken", Name: revokedID}, &token); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("compaction forgot revoked token", err)
	}
	got := ownerState(t, f.st)
	if len(got.TOTP) != 1 || got.TOTP[0] != state.TOTP[0] {
		t.Fatal("compaction lost live disabled-factor replay or retained orphan")
	}
}

func TestCompactionBindsRecreatedNameToNewUIDAcrossPromotion(t *testing.T) {
	f := newSessionFixture(t)
	oldCookie, err := f.view(t).SealSession(f.row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return Delete(ctx, tx, f.user, store.DeleteOptions{Manager: store.ManagerAPI}, f.now)
	})
	var replacement store.Object
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		var err error
		replacement, err = Apply(ctx, tx, &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: f.user.Name, CreationTimestamp: metav1.NewTime(f.now)}, Spec: f.user.Spec}, store.ApplyOptions{Manager: store.ManagerAPI}, f.now.Add(time.Second))
		return err
	})
	if replacement.GetUID() == f.user.UID {
		t.Fatal("same-name replacement reused retired UID")
	}
	state := ownerState(t, f.st)
	fillGrantHistory(&state, compactHistoryRows-1, f.now)
	seedHistory(t, f.st, state)
	row := f.row
	row.ID, row.Account, row.IssuedAt = randomID(), accountRef(replacement), f.now.Add(2*time.Second)
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		s, err := Load(ctx, tx)
		if err != nil {
			return err
		}
		if _, err := s.Enroll(row); err != nil {
			return err
		}
		return s.Commit(ctx)
	})
	newCookie, err := f.view(t).SealSession(row.ID, f.csrf)
	if err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		v := f.view(t)
		if _, err := v.CookieSession(oldCookie, f.now.Add(3*time.Second)); !errors.Is(err, servingauth.ErrCredentials) {
			t.Fatal("compaction/name reuse revived retired credential", err)
		}
		if session, err := v.CookieSession(newCookie, f.now.Add(3*time.Second)); err != nil || session.Principal.AccountUID != string(replacement.GetUID()) {
			t.Fatal("new account incarnation lost its enrollment", err)
		}
	}
	check()
	_ = f.st.Close()
	f.st = openSecurityStore(t, f.path)
	check()
	f.st = promotedCheckpointReplica(t, f.st)
	check()
}
