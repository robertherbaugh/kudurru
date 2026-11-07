package iammigrate

import (
	"context"
	"encoding/json"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	publicv1 "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Capture reads one journal cut and verifies it before returning admission and
// immutable bindings together. Native admission has no migration bindings.
// Errors and incomplete journals never return bindings or allow remote IAM.
func (m *Migrator) Capture(ctx context.Context) (Status, *Bindings, error) {
	sec, j, err := m.loadJournal(ctx)
	if err != nil {
		s, e := failed(nil, "", err)
		return s, nil, e
	}
	s, err := m.checkCut(ctx, sec, j)
	if err != nil || !s.RemoteAllowed {
		return s, nil, err
	}
	// Verification may involve multiple API reads. Do not return an allowed
	// cut if its journal was replaced or updated while those reads ran.
	if err = m.sameJournalCut(ctx, sec); err != nil {
		closed, e := failed(j, s.JournalResourceVersion, err)
		return closed, nil, e
	}
	if j == nil {
		return s, nil, nil
	}
	return s, newBindings(j.Credentials), nil
}

// Check is an uncached admission check, never a bootstrap or migration trigger.
// On every error RemoteAllowed is false. Root must invalidate admission if the
// journal changes and keep Unix recovery separate from remote authentication.
func (m *Migrator) Check(ctx context.Context) (Status, error) {
	s, _, err := m.Capture(ctx)
	return s, err
}

func (m *Migrator) checkCut(ctx context.Context, sec *corev1.Secret, j *journal) (Status, error) {
	if sec == nil {
		if _, _, err := m.identities(ctx); err != nil {
			return failed(nil, "", err)
		}
		sources, e := m.sources(ctx, nil)
		if e != nil {
			return failed(nil, "", e)
		}
		for _, s := range sources {
			if _, ok := publicResources[sourceKey(s).Kind]; ok {
				return Status{State: Required, Counts: Counts{Sources: len(sources)}}, nil
			}
			if tok, ok := s.obj.(*internalv1.APIToken); ok && accountCredential(tok) && tok.Spec.Owner.UID == "" {
				return Status{State: Required, Counts: Counts{Sources: len(sources)}}, nil
			}
		}
		if err := m.validateNative(ctx, sources); err != nil {
			return failed(nil, "", err)
		}
		return Status{State: Native, RemoteAllowed: true, Counts: Counts{Sources: len(sources)}}, nil
	}
	if j.State != Complete {
		for _, r := range j.Public {
			if r.Intent && r.UID == "" {
				target, e := m.getPublic(ctx, r.Key)
				if e != nil {
					return failed(j, sec.ResourceVersion, e)
				}
				if target != nil {
					j.State = RecoveryRequired
					return failed(j, sec.ResourceVersion, problem("uncertain-create", r.Key.Kind, r.Key.Namespace, r.Key.Name, "create"))
				}
			}
		}
		for _, r := range j.Private {
			if r.Intent && r.UID == "" {
				rctx, cancel := m.request(ctx)
				target, e := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, secretName(r.Key.Kind, "", r.Key.Name), metav1.GetOptions{})
				cancel()
				if e == nil && target != nil {
					j.State = RecoveryRequired
					return failed(j, sec.ResourceVersion, problem("uncertain-create", r.Key.Kind, "", r.Key.Name, "authority"))
				}
				if e != nil && !apierrors.IsNotFound(e) {
					return failed(j, sec.ResourceVersion, problem("api-unavailable", r.Key.Kind, "", r.Key.Name, "authority"))
				}
			}
		}
		return status(j, sec.ResourceVersion), nil
	}
	if err := m.validateComplete(ctx, j); err != nil {
		return failed(j, sec.ResourceVersion, err)
	}
	return status(j, sec.ResourceVersion), nil
}

func accountCredential(tok *internalv1.APIToken) bool {
	o := tok.Spec.Owner
	return o.Kind == internalv1.OwnerServiceAccount || (o.Kind == internalv1.OwnerUser && (o.Provider == "local" || o.ID == "local:"+o.Name))
}

func (m *Migrator) validateComplete(ctx context.Context, j *journal) error {
	p, err := m.archivedPlan(ctx, j)
	if err != nil {
		return err
	}
	targets := map[key]types.UID{}
	for _, r := range j.Public {
		targets[r.Key] = r.UID
	}
	credentials, bound, unbound, err := p.credentials(targets)
	if err != nil {
		return err
	}
	if digest(credentials) != digest(j.Credentials) || p.counts != j.Counts || bound != j.Counts.BoundTokens || unbound != j.Counts.UnboundTokens {
		return problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "bindings")
	}
	expected := map[key]string{}
	for k, admin := range p.admins {
		if !admin {
			continue
		}
		owner := accountOwner(k, targets[k])
		ps := &internalv1.PrincipalState{TypeMeta: metav1.TypeMeta{APIVersion: publicv1.GroupVersion.String(), Kind: "PrincipalState"}, ObjectMeta: metav1.ObjectMeta{Name: internalv1.PrincipalStateName(owner), Generation: 1}, Spec: internalv1.PrincipalStateSpec{Owner: owner, BuiltinAdmin: true}}
		store.SetManagedBy(ps, store.ManagerSystem)
		expected[key{Kind: "PrincipalState", Name: ps.Name}] = digest(ps)
	}
	if len(expected) != len(j.Private) {
		return problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "authority")
	}
	for _, r := range j.Private {
		if expected[r.Key] != r.Digest {
			return problem("invalid-journal", "Secret", m.opts.Namespace, JournalName, "authority")
		}
	}
	// Original public-IAM Secrets remain frozen rollback artifacts. Private
	// LocalUser/token/state records are allowed to change normally after cutover.
	legacy, err := m.listSources(ctx, nil, kindLabel+" in (IdentityProvider,RoleBinding,ServiceAccount)")
	if err != nil {
		return err
	}
	expectedSources := map[key]sourceRecord{}
	for _, r := range j.Sources {
		if _, public := publicResources[r.Key.Kind]; public {
			expectedSources[r.Key] = r
		}
	}
	if len(legacy) != len(expectedSources) {
		return problem("source-changed", "Secret", m.opts.Namespace, "", "completed-source")
	}
	for _, s := range legacy {
		r, ok := expectedSources[sourceKey(s)]
		sec := s.secret
		if !ok || sec.UID != r.UID || sec.ResourceVersion != r.RV || sourceDigest(sec) != r.Digest {
			return problem("source-changed", r.Key.Kind, r.Key.Namespace, r.Key.Name, "completed-source")
		}
	}
	return nil
}

// Bindings loads only a verified complete private journal, with no live fallback.
// Root may capture this immutable result in its complete-image auth compiler.
func (m *Migrator) Bindings(ctx context.Context) (*Bindings, error) {
	s, b, err := m.Capture(ctx)
	if err != nil {
		return nil, err
	}
	if s.State != Complete {
		return nil, problem("incomplete-migration", "Secret", m.opts.Namespace, JournalName, "bindings")
	}
	return b, nil
}

// These assertions document private serialization boundaries for callers.
var _ json.Marshaler = (*Plan)(nil)

func (m *Migrator) sameJournalCut(ctx context.Context, pinned *corev1.Secret) error {
	rctx, cancel := m.request(ctx)
	current, err := m.core.CoreV1().Secrets(m.opts.Namespace).Get(rctx, JournalName, metav1.GetOptions{})
	cancel()
	if apierrors.IsNotFound(err) && pinned == nil {
		return nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return problem("api-unavailable", "Secret", m.opts.Namespace, JournalName, "capture")
	}
	if err != nil || pinned == nil || current.UID != pinned.UID || current.ResourceVersion != pinned.ResourceVersion {
		return problem("CAS-conflict", "Secret", m.opts.Namespace, JournalName, "capture")
	}
	return nil
}

// Native private identities still need incarnation proof. Private kinds alone
// do not imply an upgrade, but an unexplained UID is never native authority.
func (m *Migrator) validateNative(ctx context.Context, sources []*source) error {
	locals := map[string]store.Object{}
	for _, s := range sources {
		if _, ok := s.obj.(*internalv1.LocalUser); ok {
			locals[s.obj.GetName()] = s.obj
		}
	}
	for _, s := range sources {
		var owner internalv1.TokenOwner
		var token *internalv1.APIToken
		switch obj := s.obj.(type) {
		case *internalv1.APIToken:
			if !accountCredential(obj) {
				continue
			}
			token = obj
			owner = obj.Spec.Owner
		case *internalv1.PrincipalState:
			owner = obj.Spec.Owner
		default:
			continue
		}
		var account store.Object
		if owner.Kind == internalv1.OwnerServiceAccount {
			k := key{"ServiceAccount", owner.Namespace, owner.Name}
			if owner.ID != "sa:"+owner.Namespace+"/"+owner.Name || (owner.Provider != "" && owner.Provider != "kudurru") {
				return problem("invalid-owner", sourceKey(s).Kind, "", s.obj.GetName(), "native")
			}
			u, err := m.getPublic(ctx, k)
			if err != nil {
				return err
			}
			if u != nil {
				account, err = publicObject(u, k)
				if err != nil {
					return err
				}
			}
		} else {
			if owner.ID != "local:"+owner.Name || owner.Namespace != "" || (owner.Provider != "" && owner.Provider != "local") {
				return problem("invalid-owner", "APIToken", "", s.obj.GetName(), "native")
			}
			account = locals[owner.Name]
		}
		if account == nil || owner.UID == "" || owner.UID != string(account.GetUID()) {
			return problem("private-conflict", sourceKey(s).Kind, "", s.obj.GetName(), "native")
		}
		created := account.GetCreationTimestamp()
		if token != nil && (created.IsZero() || token.CreationTimestamp.Before(&created)) {
			return problem("invalid-owner", "APIToken", "", token.Name, "native")
		}
	}
	return nil
}
