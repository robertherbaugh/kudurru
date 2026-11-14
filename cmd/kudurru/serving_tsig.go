package main

import (
	"context"
	"errors"
	"net/http"
	"time"

	kinds "github.com/robertherbaugh/kudurru/api/tsigkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servinghttp"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/tsigkeys"
)

const tsigKeyCommandBound = 10 * time.Second
const tsigKeyLifetimeCheck = 100 * time.Millisecond

func tsigKeyAPIMetadata(metadata tsigkeys.Metadata) api.TSIGKeyMetadata {
	result := api.TSIGKeyMetadata{KeyName: metadata.KeyName, Algorithm: metadata.Algorithm, Source: metadata.Source, UID: metadata.UID, ResourceVersion: metadata.ResourceVersion}
	if !metadata.CreatedAt.IsZero() {
		created := metadata.CreatedAt.UTC().Truncate(time.Second)
		result.CreatedAt = &created
	}
	return result
}

// TSIGKeyReads projects only safe original-cut metadata. No live source or file
// fallback repairs an unavailable complete runtime or superseded protected read.
func (s *servingRuntime) TSIGKeyReads(ctx context.Context) ([]api.TSIGKeyMetadata, error) {
	if s == nil || ctx == nil || s.opts.TSIG == nil {
		return nil, tsigKeyUnsupported()
	}
	lease, err := servinghttp.CloneLease(ctx)
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	defer lease.Release()
	owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	metadata, err := owner.TSIGMetadata()
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	result := make([]api.TSIGKeyMetadata, len(metadata))
	for i, key := range metadata {
		result[i] = tsigKeyAPIMetadata(key)
	}
	if err := s.publisher.CheckReadLease(ctx, lease); err != nil {
		return nil, tsigKeyAPIError(err)
	}
	return result, nil
}

// TSIGKeyCommand plans once against original immutable inputs, authorizes and
// stages in the actual current command, then observes only that committed target.
// An error is not a rollback certificate: SQL completion may be uncertain.
func (s *servingRuntime) TSIGKeyCommand(ctx context.Context, change api.TSIGKeyChange) (*api.TSIGKeyCommandReceipt, error) {
	if s == nil || ctx == nil || s.opts.TSIG == nil {
		return nil, tsigKeyUnsupported()
	}
	ctx, cancel := context.WithTimeout(ctx, tsigKeyCommandBound)
	defer cancel()
	change.KeyName = kinds.CanonicalName(change.KeyName)
	attrs, err := tsigKeyAttributes(change)
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	raw, err := servinghttp.Credential(ctx)
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	original, err := servinghttp.Component[*servingauth.View](ctx, "auth")
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	owner, err := servinghttp.Component[*servingbuild.RuntimeOwner](ctx, "runtime")
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	proof, err := servingauth.CaptureOperation(ctx, original, raw, attrs, s.opts.Now())
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	if !owner.TSIGOriginalCurrent() {
		return nil, tsigKeyAPIError(serving.ErrDependency)
	}
	plan, err := owner.PlanTSIGChange(tsigkeys.MutationInput{Operation: tsigkeys.Operation(change.Operation), KeyName: change.KeyName, Algorithm: change.Algorithm, UID: change.UID, ResourceVersion: change.ResourceVersion, Secret: change.Secret})
	if err != nil {
		return nil, tsigKeyAPIError(err)
	}
	// Retain one original episode as an additional memory-only final check;
	// currentAuthCommandThen still performs all actual binding/admission.
	writer := s.primary.writer.active.Load()
	target := tsigKeyPublication{original: owner, proof: proof, raw: raw, attrs: attrs, dependency: original.DependencyIdentity(), encryption: owner.TSIGEncryptionIdentity(), writerCurrent: func() bool { return s.primary.writer.current(writer) }}
	if err := target.checkOriginal(ctx, s.opts.Now()); err != nil {
		return nil, tsigKeyAPIError(err)
	}
	var metadata tsigkeys.Metadata
	observed := false
	committed, err := s.currentAuthCommandThen(ctx, func(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies) error {
		principal, err := servingauth.ReauthorizeOperationCurrent(ctx, tx, deps, proof, s.opts.Now)
		if err != nil {
			return err
		}
		if err := target.checkOriginal(ctx, s.opts.Now()); err != nil {
			return err
		}
		metadata, err = plan.Stage(ctx, tx)
		if err != nil {
			return err
		}
		// Capture the complete intended source in this SAME transaction. The
		// typed stage's final journal fence bars any later actual TSIG change.
		post, err := tsigkeys.ReadTx(ctx, tx)
		if err != nil {
			return err
		}
		target.source, target.principal = post.Identity(), tsigPrincipalBinding(principal)
		if err := tx.RecordAudit(ctx, sqlite.AuditEntry{Action: "tsigkey." + string(change.Operation), Key: store.Key{Kind: kinds.KindTSIGKey, Name: attrs.Name}, ResourceVersion: metadata.ResourceVersion, Manager: store.ManagerAPI, Reason: "stored TSIG lifecycle command"}); err != nil {
			return err
		}
		// Keep the original narrow lifetime too; this is still PRECOMMIT and
		// cannot replace complete source/proof/writer admission or its pins.
		return tx.RequireCommitCheck(func(ctx context.Context) error { return target.checkOriginal(ctx, s.opts.Now()) })
	}, func(ctx context.Context, writer serving.TermActivation) error {
		s.publisher.Wake()
		if err := s.awaitTSIGKeyPublication(ctx, writer, target); err != nil {
			return err
		}
		observed = true
		return nil
	})
	if !committed {
		return nil, tsigKeyAPIError(err)
	}
	// currentAuthCommandThen performed its final ORIGINAL episode check. Never
	// credit an earlier match after that check failed or original time ended.
	published := err == nil && observed && target.checkOriginal(ctx, s.opts.Now()) == nil
	return &api.TSIGKeyCommandReceipt{Operation: change.Operation, Committed: true, Published: published, Removed: change.Operation == api.TSIGKeyRevoke, Key: tsigKeyAPIMetadata(metadata)}, nil
}

func tsigKeyAttributes(change api.TSIGKeyChange) (rbac.Attributes, error) {
	name := kinds.ObjectName(change.KeyName)
	if name == "" || len(change.UID) > 256 || len(change.ResourceVersion) > 256 || len(change.Algorithm) > 64 || len(change.Secret) > tsigkeys.MaxSecretBytes {
		return rbac.Attributes{}, tsigkeys.ErrIntent
	}
	verb := "write"
	switch change.Operation {
	case api.TSIGKeyCreate, api.TSIGKeyReplace, api.TSIGKeyReseal:
	case api.TSIGKeyRevoke:
		verb = "delete"
	default:
		return rbac.Attributes{}, tsigkeys.ErrIntent
	}
	return rbac.Attributes{Resource: "tsigkeys", Verb: verb, Scope: rbac.Scope{Kind: rbac.ScopeCluster}, Name: name}, nil
}

// This private tuple mirrors the original operation identity, with external
// absence explicit. Grants/groups are checked by the observed View.Authorize;
// display names or raw principal comparison cannot replace these owner bindings.
type tsigKeyPrincipal struct {
	kind                                                       auth.PrincipalKind
	id, name, provider, account, serviceAccount, token, method string
	issued                                                     time.Time
	external                                                   bool
	providerUID, origin, session                               string
}

func tsigPrincipalBinding(principal auth.Principal) tsigKeyPrincipal {
	binding := tsigKeyPrincipal{kind: principal.Kind, id: principal.ID, name: principal.Name, provider: principal.Provider, account: principal.AccountUID, serviceAccount: principal.ServiceAccount, token: principal.TokenID, method: principal.AuthMethod, issued: principal.CredentialIssuedAt}
	if principal.External != nil {
		binding.external = true
		binding.providerUID, binding.origin, binding.session = principal.External.ProviderUID, principal.External.Origin, principal.External.SessionID
	}
	return binding
}

type tsigKeyPublication struct {
	original                       *servingbuild.RuntimeOwner
	proof                          *servingauth.OperationProof
	raw                            servingauth.Credential
	attrs                          rbac.Attributes
	principal                      tsigKeyPrincipal
	dependency, source, encryption string
	writerCurrent                  func() bool
}

func (tsigKeyPublication) String() string               { return "private TSIG publication target" }
func (p tsigKeyPublication) GoString() string           { return p.String() }
func (tsigKeyPublication) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }
func (tsigKeyPrincipal) String() string                 { return "private TSIG operation identity" }
func (p tsigKeyPrincipal) GoString() string             { return p.String() }
func (tsigKeyPrincipal) MarshalJSON() ([]byte, error)   { return []byte(`{"private":true}`), nil }

func (p tsigKeyPublication) checkOriginal(ctx context.Context, now time.Time) error {
	if err := p.proof.CheckOriginal(ctx, now); err != nil {
		return err
	}
	if p.original == nil || !p.original.TSIGOriginalCurrent() {
		return serving.ErrDependency
	}
	if p.writerCurrent == nil || !p.writerCurrent() {
		return replication.ErrFenced
	}
	return nil
}

func (s *servingRuntime) awaitTSIGKeyPublication(ctx context.Context, writer serving.TermActivation, target tsigKeyPublication) error {
	if target.source == "" || target.encryption == "" || target.dependency == "" {
		return serving.ErrComponents
	}
	ticker := time.NewTicker(tsigKeyLifetimeCheck)
	defer ticker.Stop()
	for {
		changed, authority := s.publisher.Subscribe(), s.publisher.AuthorityChanges()
		if err := target.checkOriginal(ctx, s.opts.Now()); err != nil {
			return err
		}
		if err := s.writerCurrent(ctx, writer); err != nil {
			return err
		}
		lease, err := s.publisher.CaptureHTTP(ctx)
		if err == nil {
			ready, captureErr := s.tsigKeyPublication(ctx, lease, writer, target)
			lease.Release()
			if captureErr != nil {
				return captureErr
			}
			if ready {
				return nil
			}
		} else if !errors.Is(err, serving.ErrPending) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		case <-authority:
		case <-ticker.C:
		}
	}
}

func (s *servingRuntime) tsigKeyPublication(ctx context.Context, lease *serving.Lease, writer serving.TermActivation, target tsigKeyPublication) (bool, error) {
	if err := target.checkOriginal(ctx, s.opts.Now()); err != nil {
		return false, err
	}
	bundle := lease.Bundle()
	if bundle == nil {
		return false, serving.ErrReleased
	}
	runtimeRaw, runtimeOK := bundle.Component("runtime")
	owner, validRuntime := runtimeRaw.(*servingbuild.RuntimeOwner)
	authRaw, authOK := bundle.Component("auth")
	view, validAuth := authRaw.(*servingauth.View)
	if !runtimeOK || !validRuntime || owner == nil || !authOK || !validAuth || view == nil || !view.Base().Same(bundle.Identity().Base) {
		return false, serving.ErrComponents
	}
	if view.DependencyIdentity() != target.dependency {
		return false, servingauth.ErrDependency
	}
	if !owner.MatchesTSIGTarget(target.source, target.encryption) {
		return false, nil
	}
	now := s.opts.Now()
	if err := target.checkOriginal(ctx, now); err != nil {
		return false, err
	}
	// Reverify the same raw proof against this immutable observed cut. This
	// performs no session touch, Store/network lookup or new CaptureOperation.
	principal, err := view.Authenticate(ctx, target.raw, now)
	if err != nil {
		return false, err
	}
	if tsigPrincipalBinding(principal) != target.principal {
		return false, servingauth.ErrCredentials
	}
	decision, err := view.Authorize(principal, target.attrs, now)
	if err != nil {
		return false, err
	}
	if !decision.Allowed {
		return false, servingauth.ErrDenied
	}
	if err := s.publisher.CheckWriterLease(ctx, lease, writer); err != nil {
		if errors.Is(err, serving.ErrPending) {
			return false, nil
		}
		return false, err
	}
	if err := target.checkOriginal(ctx, s.opts.Now()); err != nil {
		return false, err
	}
	return true, nil
}

// tsigKeyUnsupported answers a runtime that was constructed without its stored
// TSIG source. Both storage drivers mount stored-key management in runServe, so
// no production request reaches it; it keeps a miswired runtime from serving
// an unchecked command.
func tsigKeyUnsupported() error {
	return &api.Error{Status: http.StatusNotImplemented, Body: api.ErrorBody{Key: api.KeyNotImplemented, Message: "durable stored TSIG key management is not available on this node"}}
}

// No raw SQL/provider/credential errors or retry advice cross this boundary.
// In particular, an uncertain SQL completion must not imply durable rollback.
func tsigKeyAPIError(err error) error {
	if refusal, ok := asPrimaryRequired(err); ok {
		return refusal
	}
	switch {
	case errors.Is(err, tsigkeys.ErrIntent), errors.Is(err, tsigkeys.ErrInvalid), errors.Is(err, tsigkeys.ErrBounds), errors.Is(err, servingauth.ErrBounds):
		return &api.Error{Status: http.StatusBadRequest, Body: api.ErrorBody{Key: api.KeyInvalid, Message: "invalid stored TSIG key command"}}
	case errors.Is(err, tsigkeys.ErrPrecondition), errors.Is(err, tsigkeys.ErrFileKey), errors.Is(err, store.ErrConflict):
		return &api.Error{Status: http.StatusConflict, Body: api.ErrorBody{Key: api.KeyConflict, Message: "stored TSIG key precondition or read-only source conflict"}}
	case errors.Is(err, servingauth.ErrCredentials):
		return &api.Error{Status: http.StatusUnauthorized, Body: api.ErrorBody{Key: api.KeyUnauthenticated, Message: "original credential is invalid or expired"}}
	case errors.Is(err, servingauth.ErrDenied):
		return &api.Error{Status: http.StatusForbidden, Body: api.ErrorBody{Key: api.KeyForbidden, Message: "current authority does not permit this TSIG command"}}
	case errors.Is(err, replication.ErrReadOnly), errors.Is(err, replication.ErrFenced), errors.Is(err, tsigkeys.ErrSource), errors.Is(err, serving.ErrDependency), errors.Is(err, servingauth.ErrDependency), errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "original TSIG command authority or source unavailable; inspect fresh metadata before further action"}}
	default:
		return &api.Error{Status: http.StatusServiceUnavailable, Body: api.ErrorBody{Key: api.KeyUnavailable, Message: "TSIG command outcome unavailable; inspect fresh metadata before further action"}}
	}
}
