package security

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/api/runtimekinds"
	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/auth/tokens"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func externalAdminRow(t *testing.T, f *externalIssueFixture) servingauth.ExternalAccountInfo {
	t.Helper()
	rows, _, err := f.view(t).ExternalAccounts()
	if err != nil || len(rows) != 1 {
		t.Fatal("expected exact enrolled account", err)
	}
	return rows[0]
}

func TestExternalAdminAtomicRevisionRevocationRestartPromotion(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	now := f.now.Add(time.Second)
	proof := f.proof(t, now, "alice", "ops")
	enrollment := f.issue(t, proof, now)
	cookie, _, err := enrollment.Seal(f.view(t), now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	original := externalAdminRow(t, f)
	mutate := func(row servingauth.ExternalAccountInfo, action string, at time.Time, abort bool) error {
		return f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if err := AdministerExternal(ctx, tx, row.Name, row.Identity.UID, row.Revision, action, at); err != nil {
				return err
			}
			if abort {
				return tx.RequireCommitCheck(func(context.Context) error { return servingauth.ErrCredentials })
			}
			return nil
		})
	}
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	audits, err := f.st.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if err := mutate(original, "disable", now.Add(2*time.Second), true); !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal("final denial failed", err)
	}
	after, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rolledAudit, err := f.st.ReadAudit(t.Context(), sqlite.AuditQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !before.Generation().Same(after.Generation()) || len(audits) != len(rolledAudit) {
		t.Fatal("abort leaked state or audit")
	}
	if _, err := f.view(t).CookieSession(cookie, now.Add(3*time.Second)); err != nil {
		t.Fatal("rollback revoked credential", err)
	}
	wrong := original
	wrong.Identity.UID = "ffffffffffffffffffffffffffffffff"
	if err := mutate(wrong, "disable", now.Add(3*time.Second), false); !errors.Is(err, store.ErrConflict) {
		t.Fatal("replacement UID accepted", err)
	}
	if err := mutate(original, "disable", now.Add(3*time.Second), false); err != nil {
		t.Fatal(err)
	}
	disabled := externalAdminRow(t, f)
	if !disabled.Identity.Disabled || disabled.Revision == original.Revision || disabled.ValidAfter.IsZero() {
		t.Fatal("missing disable or revision/floor")
	}
	if err := mutate(original, "enable", now.Add(4*time.Second), false); !errors.Is(err, store.ErrConflict) {
		t.Fatal("stale review accepted", err)
	}
	if err := mutate(disabled, "enable", now.Add(4*time.Second), false); err != nil {
		t.Fatal(err)
	}
	enabled := externalAdminRow(t, f)
	if enabled.Identity.Disabled || enabled.Revision == original.Revision || !enabled.ValidAfter.Equal(disabled.ValidAfter) {
		t.Fatal("enable lost revocation history")
	}
	for _, state := range []string{"enabled", "restarted", "promoted"} {
		if state == "restarted" {
			if err := f.st.Close(); err != nil {
				t.Fatal(err)
			}
			f.st = openSecurityStore(t, f.path)
		}
		if state == "promoted" {
			opts := sqlite.Options{Scheme: f.registry.Scheme, InitialMode: replication.Follower, ValidateReplicaEntry: servingauth.ValidateEntry}
			for _, kind := range runtimekinds.Private() {
				opts.InternalKinds = append(opts.InternalKinds, store.Kind(kind))
			}
			follower, err := sqlite.Open(filepath.Join(t.TempDir(), "replica.db"), opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = follower.Close() })
			if err := follower.Enroll(t.Context(), f.st.Authority().Term); err != nil {
				t.Fatal(err)
			}
			installCtx := captureSchema8Install(t.Context(), t, follower)
			image, err := f.st.ReadGeneration(installCtx)
			if err != nil {
				t.Fatal(err)
			}
			validated, err := follower.ValidateImage(installCtx, *image)
			if err != nil {
				t.Fatal(err)
			}
			authority := follower.Authority()
			installed, err := follower.InstallReplica(installCtx, validated, replication.InstallOptions{ExpectedTerm: authority.Term, ExpectedInstalled: authority.Installed})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := follower.Promote(t.Context(), replication.PromoteRequest{ExpectedTerm: image.Term, ExpectedInstalled: installed, RequestID: "external-admin-promotion"}); err != nil {
				t.Fatal(err)
			}
			f.st = follower
		}
		if _, err := f.view(t).CookieSession(cookie, now.Add(5*time.Second)); err == nil {
			t.Fatal(state, "revived revoked cookie")
		}
		if err := f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			_, err := IssueExternal(ctx, tx, f.deps, proof, func() time.Time { return now.Add(5 * time.Second) })
			return err
		}); !errors.Is(err, servingauth.ErrCredentials) {
			t.Fatal(state, "old JWT reissued session", err)
		}
	}
	// A newly verified proof can enroll again after enable, but logout still
	// revokes that exact account without toggling its disabled state.
	at := now.Add(10 * time.Second)
	fresh := f.issue(t, f.proof(t, at, "alice", "ops"), at)
	freshCookie, _, err := fresh.Seal(f.view(t), at.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if err := mutate(externalAdminRow(t, f), "logout", at.Add(2*time.Second), false); err != nil {
		t.Fatal(err)
	}
	if externalAdminRow(t, f).Identity.Disabled {
		t.Fatal("logout disabled account")
	}
	if _, err := f.view(t).CookieSession(freshCookie, at.Add(3*time.Second)); err == nil {
		t.Fatal("logout retained session")
	}
}

func TestExternalAdminDoesNotRevokeSameUsernameForeignOwners(t *testing.T) {
	f := newExternalIssueFixture(t, true)
	now := f.now.Add(time.Second)
	f.issue(t, f.proof(t, now, "alice", "ops"), now)
	row := externalAdminRow(t, f)
	owner := internalv1.TokenOwner{Kind: internalv1.OwnerUser, UID: row.Identity.UID, ID: (oidccore.Identity{Provider: row.Identity.Provider.ID, Issuer: row.Identity.Issuer, Subject: row.Identity.Subject}).ID(), Provider: row.Identity.Provider.ID, Name: row.Identity.Username}
	names := map[string]string{}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		for _, mode := range []string{"exact", "renamed", "other-uid", "other-provider", "other-subject", "local"} {
			o := owner
			switch mode {
			case "renamed":
				o.Name = "old-name"
			case "other-uid":
				o.UID = "ffffffffffffffffffffffffffffffff"
			case "other-provider":
				o.Provider = "different"
			case "other-subject":
				o.ID = (oidccore.Identity{Provider: o.Provider, Issuer: row.Identity.Issuer, Subject: "same-name-other-subject"}).ID()
			case "local":
				o.Provider = "local"
				o.ID = "local:alice"
			}
			wire, id := tokens.Generate()
			names[mode] = id
			if _, err := tx.Apply(ctx, &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id, CreationTimestamp: metav1.NewTime(now)}, Spec: internalv1.APITokenSpec{SecretHash: tokens.Hash(wire), Owner: o, Grants: []string{"viewer"}}}, store.ApplyOptions{Manager: store.ManagerSystem}); err != nil {
				return err
			}
		}
		return nil
	})
	before, err := f.st.ReadGeneration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
		if err := AdministerExternal(ctx, tx, row.Name, row.Identity.UID, row.Revision, "logout", now.Add(time.Second)); err != nil {
			return err
		}
		return tx.RequireCommitCheck(func(context.Context) error { return servingauth.ErrCredentials })
	})
	if !errors.Is(err, servingauth.ErrCredentials) {
		t.Fatal(err)
	}
	after, err := f.st.ReadGeneration(t.Context())
	if err != nil || !before.Generation().Same(after.Generation()) {
		t.Fatal("partial token deletion retained", err)
	}
	command(t, f.st, func(ctx context.Context, tx *sqlite.CommandTx) error {
		return AdministerExternal(ctx, tx, row.Name, row.Identity.UID, row.Revision, "logout", now.Add(2*time.Second))
	})
	for mode, id := range names {
		var token internalv1.APIToken
		err := f.st.Get(t.Context(), store.Key{Kind: "APIToken", Name: id}, &token)
		if mode == "exact" || mode == "renamed" {
			if !errors.Is(err, store.ErrNotFound) {
				t.Fatal("exact immutable owner retained", mode, err)
			}
		} else if err != nil {
			t.Fatal("foreign account sharing profile revoked", mode, err)
		}
	}
}

func TestExternalAdminOriginalCredentialFinalTimeIncludingNoop(t *testing.T) {
	for _, action := range []string{"disable", "enable"} {
		for _, regress := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/regress=%t", action, regress), func(t *testing.T) {
				f := newExternalIssueFixture(t, true)
				at := f.now.Add(time.Second)
				f.deps.StaticBindings = []rbac.Binding{{Role: "admin", Subjects: []rbac.Subject{{Kind: rbac.SubjectGroup, Name: "corp:ops"}}}}
				proof := f.proof(t, at, "alice", "ops")
				f.issue(t, proof, at)
				row := externalAdminRow(t, f)
				before, err := f.st.ReadGeneration(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				audits, err := f.st.ReadAudit(t.Context(), sqlite.AuditQuery{})
				if err != nil {
					t.Fatal(err)
				}
				clock := at.Add(time.Second)
				err = f.st.Command(t.Context(), f.st.Authority().Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
					_, _, err := servingauth.ReauthorizeCurrentWithClock(ctx, tx, f.deps, servingauth.Credential{Kind: servingauth.OIDCJWT, Value: proof.RawIDToken}, rbac.Attributes{Resource: "users", Verb: "write", Scope: rbac.Scope{Kind: rbac.ScopeCluster}}, func() time.Time { return clock })
					if err != nil {
						return err
					}
					if err := AdministerExternal(ctx, tx, row.Name, row.Identity.UID, row.Revision, action, clock); err != nil {
						return err
					}
					if regress {
						clock = at.Add(-time.Nanosecond)
					} else {
						clock = at.Add(5 * time.Minute)
					}
					return nil
				})
				if !errors.Is(err, servingauth.ErrCredentials) {
					t.Fatal("original raw proof final time accepted", err)
				}
				after, err := f.st.ReadGeneration(t.Context())
				if err != nil || !before.Generation().Same(after.Generation()) {
					t.Fatal("expired command persisted changes", err)
				}
				afterAudit, err := f.st.ReadAudit(t.Context(), sqlite.AuditQuery{})
				if err != nil || len(audits) != len(afterAudit) {
					t.Fatal("expired command persisted audit", err)
				}
			})
		}
	}
}
