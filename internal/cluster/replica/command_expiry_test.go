package replica

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

type finalCheckDomain struct {
	*fixtureDomain
	register func(CommandTx) error
}

func (d finalCheckDomain) Authorize(ctx context.Context, tx CommandTx, c Command) (context.Context, error) {
	ctx, err := d.fixtureDomain.Authorize(ctx, tx, c)
	if err != nil {
		return nil, err
	}
	if err := d.register(tx); err != nil {
		return nil, err
	}
	return ctx, nil
}

func TestCommandFinalExpiryRollsBackMutationAndRefusesDedup(t *testing.T) {
	for _, replay := range []bool{false, true} {
		t.Run(map[bool]string{false: "mutation", true: "dedup-replay"}[replay], func(t *testing.T) {
			s := openStore(t, replication.Primary)
			cmd := commandFor(s, "expires-at-final-admission")
			domain := &fixtureDomain{}
			if replay {
				if _, err := serviceFor(s, domain).Execute(t.Context(), cmd); err != nil {
					t.Fatal(err)
				}
			}
			before := imageOf(t, s)
			auditBefore, err := s.ReadAudit(t.Context(), sqlite.AuditQuery{})
			if err != nil {
				t.Fatal(err)
			}
			now, checked := cmd.IssuedAt, false
			d := finalCheckDomain{fixtureDomain: domain, register: func(tx CommandTx) error {
				return tx.State().(*sqlite.CommandTx).RequireCommitCheck(func(context.Context) error {
					checked = true
					now = cmd.NotAfter
					return nil
				})
			}}
			service := &CommandService{Backend: SQLiteAtomic{Store: s}, Domain: d, Now: func() time.Time { return now }}
			out, err := service.Execute(t.Context(), cmd)
			if !checked || !errors.Is(err, ErrExpired) || out.Status != 0 || len(out.Body) != 0 {
				t.Fatal("envelope expired at the actual final boundary without refusing its outcome", checked, err, out.Status)
			}
			if domain.executions.Load() != 1 || !imageOf(t, s).Generation().Same(before.Generation()) {
				t.Fatal("final expiry leaked a mutation, revision or dedup row, or replayed the domain")
			}
			auditAfter, err := s.ReadAudit(t.Context(), sqlite.AuditQuery{})
			if err != nil || len(auditAfter) != len(auditBefore) {
				t.Fatal("final expiry leaked an audit row", err)
			}
		})
	}
}

func TestCommandFinalCheckCapacityCannotBypassExpiryGuard(t *testing.T) {
	s := openStore(t, replication.Primary)
	before := imageOf(t, s)
	checked := false
	d := finalCheckDomain{fixtureDomain: &fixtureDomain{}, register: func(tx CommandTx) error {
		for range sqlite.MaxCommitChecks {
			if err := tx.State().(*sqlite.CommandTx).RequireCommitCheck(func(context.Context) error { checked = true; return nil }); err != nil {
				return err
			}
		}
		return nil
	}}
	service := &CommandService{Backend: SQLiteAtomic{Store: s}, Domain: d}
	if _, err := service.Execute(t.Context(), commandFor(s, "full-check-capacity")); err == nil {
		t.Fatal("missing final envelope guard did not refuse the command")
	}
	if checked || d.executions.Load() != 1 || !imageOf(t, s).Generation().Same(before.Generation()) {
		t.Fatal("final-check registration refusal leaked transaction effects")
	}
}
