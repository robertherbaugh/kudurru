package main

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

type integrationOperationKey struct{}

// integrationOperation lasts only for one synchronous HTTP handler. It retains
// initial latest-publication admission while each distinct effect independently
// checks the same proof against its actual transaction. It is never a credential.
type integrationOperation struct {
	runtime       *integrationRuntime
	view          *servingauth.View
	proof         *servingauth.OperationProof
	writer        *primaryWriterTerm
	capture       *ih.CommandCapture
	guard         ih.ExecutionGuard
	cut           *integrationCut
	ctx           context.Context
	release       func()
	once          sync.Once
	closed        atomic.Bool
	finishStarted atomic.Bool
	// The host worker writes this before returning its answer on the command
	// channel. The HTTP goroutine reads it only after that synchronous answer.
	status atomic.Pointer[ih.InstanceStatus]
	// principal is the reauthorized caller of the current final transaction,
	// set by integrationRuntime.command before it applies the transaction.
	principal auth.Principal
}

// integrationAuditRow is the audit row of a reviewed sync or act (D379).
type integrationAuditRow struct {
	action, name string
	uncertain    bool
	// params is the digest of an act's parameters (D401), so the row
	// identifies what was asked without storing caller-supplied values.
	params string
	// subject is what the act affects as the provider names it in
	// canonical form (ActionAuditor, D384), for example "client
	// 00:11:22:33:44:55", so that rows of one client can be found.
	subject string
}

func (*integrationOperation) String() string     { return "private integration operation" }
func (o *integrationOperation) GoString() string { return o.String() }

func (r *integrationRuntime) beginOperation(ctx context.Context, owner ih.InstanceID, revision, verb string) (*integrationOperation, error) {
	if r == nil || r.s == nil || ctx == nil || owner.Validate() != nil || revision == "" || len(revision) > 256 || owner.ClusterID != r.s.opts.Store.Info().ClusterID {
		return nil, ih.ErrInvalid
	}
	if verb != "read" && verb != "check" && verb != "sync" && verb != "act" {
		return nil, ih.ErrInvalid
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, err
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, err
	}
	objects, err := servinghttp.Component[*zonecache.Snapshot](ctx, "objects")
	if err != nil {
		return nil, err
	}
	original, ok := objects.Objects.Get(store.Key{Kind: "Integration", Namespace: owner.Namespace, Name: owner.Name})
	if !ok || string(original.GetUID()) != owner.UID {
		return nil, ih.ErrFenced
	}
	attrs := rbac.Attributes{Resource: "integrations", Verb: verb, Namespace: owner.Namespace, Name: owner.Name, Scope: rbac.Scope{Kind: rbac.ScopeCluster}}
	proof, err := servingauth.CaptureOperation(ctx, view, raw, attrs, r.s.opts.Now())
	if err != nil {
		return nil, err
	}
	writer := r.s.primary.writer.active.Load()
	if !r.s.primary.writer.current(writer) {
		return nil, ih.ErrFenced
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	ctx, release, err := r.s.primary.writer.bind(ctx, writer.fence)
	if err != nil {
		cancel()
		return nil, err
	}
	op := &integrationOperation{runtime: r, view: view, proof: proof, writer: writer, release: func() { release(); cancel() }}
	accepted := false
	defer func() {
		if !accepted {
			op.close()
		}
	}()
	op.capture, err = r.host.CaptureCommand(ctx, owner, revision)
	if err != nil {
		return nil, err
	}
	// The host accepts a review of an earlier revision that differs from its
	// frame only by status writes (D384). This node's published object must
	// still be the reviewed revision or that frame's.
	if rv := original.GetResourceVersion(); rv != revision && rv != op.capture.Revision() {
		return nil, ih.ErrFenced
	}
	ctx, op.guard, err = op.capture.Bind(ctx)
	if err != nil {
		return nil, err
	}
	ctx, _, err = r.requestContext(ctx, op.guard.Ledger)
	if err != nil {
		return nil, err
	}
	op.cut, _ = ctx.Value(integrationRequestKey{}).(*integrationCut)
	op.ctx = context.WithValue(ctx, integrationOperationKey{}, op)
	if err = op.credentialsCurrent(op.ctx); err != nil {
		return nil, err
	}
	err = r.host.WithLease(op.ctx, op.guard.Ledger, func(ctx context.Context) error {
		r.mu.RLock()
		defer r.mu.RUnlock()
		cut, err := r.original(ctx, op.guard)
		if err != nil || cut != op.cut {
			return ih.ErrFenced
		}
		// This first no-op retains the ordinary latest HTTP publication check.
		// It is never repeated after the operation's own budget/source writes.
		return r.s.currentAuthCommand(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
			if err := op.check(ctx); err != nil {
				return err
			}
			if err := tx.RequireCommitCheck(op.check); err != nil {
				return err
			}
			if _, err := servingauth.ReauthorizeOperationCurrent(ctx, tx, deps, proof, r.s.opts.Now); err != nil {
				return err
			}
			return integrationInputsCurrent(ctx, tx, cut, op.guard)
		})
	})
	if err != nil {
		return nil, err
	}
	accepted = true
	return op, nil
}

func (o *integrationOperation) close() {
	if o == nil {
		return
	}
	o.once.Do(func() {
		o.closed.Store(true)
		if o.release != nil {
			o.release()
		}
	})
}

// check performs no I/O or host lock acquisition. Its caller holds the original
// host replacement barrier and the coordinator's dependency/writer exclusions.
func (o *integrationOperation) check(ctx context.Context) error {
	if o == nil || o.runtime == nil || o.closed.Load() || o.cut == nil || ctx == nil || ctx.Value(integrationOperationKey{}) != o {
		return ih.ErrFenced
	}
	if err := o.proof.CheckOriginal(ctx, o.runtime.s.opts.Now()); err != nil {
		return err
	}
	if err := o.capture.CheckLifetime(ctx); err != nil {
		return err
	}
	view, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil || view != o.view {
		return ih.ErrFenced
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || token != o.writer.token || !o.runtime.s.primary.writer.current(o.writer) || !o.cut.license.Current() || !o.cut.keysCurrent() {
		return ih.ErrFenced
	}
	return o.runtime.s.writerCurrent(ctx, writerTarget(token))
}

// credentialsCurrent resolves only original installed references, outside every
// gate. Changed bytes refuse; they never replace the retained provider secrets.
func (o *integrationOperation) credentialsCurrent(ctx context.Context) error {
	if err := o.check(ctx); err != nil {
		return err
	}
	if err := o.capture.Current(ctx); err != nil {
		return err
	}
	i := o.cut.instances[o.guard.Ledger.Owner]
	if i == nil || o.cut.credentialResolver == nil {
		return ih.ErrFenced
	}
	expected := i.CredentialFingerprints()
	for name, ref := range i.Definition().Spec.Credentials {
		value, err := o.cut.credentialResolver.Resolve(ctx, ref)
		clear(value.Bytes)
		if err != nil || value.Fingerprint == "" || value.Fingerprint != expected[name] {
			return ih.ErrCredentials
		}
	}
	return o.check(ctx)
}

// finish is one final current transaction, including deferred status. A failed
// acknowledgment does not roll back earlier external effects or replay them.
// A reviewed sync or act records its audit row in this same transaction
// (D379): when the transaction is refused, neither the status nor the row
// exists, and the caller reports the outcome as uncertain.
func (o *integrationOperation) finish(audit *integrationAuditRow) error {
	if o == nil || !o.finishStarted.CompareAndSwap(false, true) {
		return ih.ErrInvalid
	}
	defer o.close()
	if err := o.credentialsCurrent(o.ctx); err != nil {
		return err
	}
	r := o.runtime
	err := r.host.WithLease(o.ctx, o.guard.Ledger, func(ctx context.Context) error {
		return r.command(ctx, o.guard, func(ctx context.Context, tx *sqlite.CommandTx, cut *integrationCut) error {
			if audit != nil {
				if err := tx.RecordAudit(ctx, o.auditEntry(cut, *audit)); err != nil {
					return err
				}
			}
			// An uncertain outcome records only what was asked; the status
			// of a refused or unacknowledged effect is not staged.
			if status := o.status.Load(); status != nil && (audit == nil || !audit.uncertain) {
				return stageIntegrationStatus(ctx, tx, cut, o.guard, *status)
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	// Our final status may advance the source. Check only original lifetime,
	// writer and time here; current proof was checked in that same final tx.
	return o.check(o.ctx)
}

func (o *integrationOperation) auditEntry(cut *integrationCut, a integrationAuditRow) sqlite.AuditEntry {
	owner := o.guard.Ledger.Owner
	outcome, result := "completed", sqlite.AuditSuccess
	if a.uncertain {
		outcome, result = "uncertain: effects may have occurred and were not replayed", sqlite.AuditError
	}
	subject := "reviewed sync"
	if a.action == "integration.act" {
		subject = "reviewed action " + a.name
		if a.subject != "" {
			subject += " for " + a.subject
		}
		if a.params != "" {
			subject += " with params " + a.params
		}
	}
	revision := ""
	if i := cut.instances[owner]; i != nil {
		revision = i.Definition().ResourceVersion
	}
	return sqlite.AuditEntry{Action: a.action, Actor: string(o.principal.Kind) + ":" + o.principal.Name, Authz: "integrations:" + o.proofVerb(a), Key: store.Key{Kind: "Integration", Namespace: owner.Namespace, Name: owner.Name}, ResourceVersion: revision, Manager: store.ManagerAPI, Result: result, Reason: subject + " of uid " + owner.UID + ": " + outcome}
}

func (*integrationOperation) proofVerb(a integrationAuditRow) string {
	if a.action == "integration.act" {
		return "act"
	}
	return "sync"
}
