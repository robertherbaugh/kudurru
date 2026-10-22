// Package sqliteadapter admits committed-primary commands through one actual
// SQLite CommandTx. It has no Kubernetes adapter or preflight leader fallback.
package sqliteadapter

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// CurrentCredential binds public identity metadata and a policy validator to
// the SAME immutable generation held by CredentialGuard through the command.
type CurrentCredential struct {
	Principal                  dynamicupdate.Principal
	ProviderVersion, Algorithm string
	Policy                     PolicyGuard
}

// CredentialGuard pins root's current immutable provider publication throughout
// fn, INCLUDING Store.Command's final commit/no-op fence. It invokes fn exactly
// once synchronously and returns its result unchanged. No network/Store reentry,
// key bytes, mutable live lookup or verification-time version substitution.
type CredentialGuard interface {
	WithCurrent(context.Context, pr.ProofIdentity, func(CurrentCredential) error) error
}

// PolicyGuard validates the complete current desired UPDATE policy/credential
// versions in the same transaction. It must use only tx reads and the immutable
// generation pinned by CredentialGuard. CurrentCredential.Policy is mandatory;
// Options.Policy can only add another conjunct. Background commands instead
// require the pinned BackgroundGuard validator.
type PolicyGuard func(context.Context, *sqlite.CommandTx, *public.DNSZone, zonestate.Policy) error

// BackgroundCheck describes the exact transaction's lifecycle/source and policy.
// Source is nil only for an admitted removed/former-UID source. Policy is nil
// for lifecycle-only membership, otherwise the candidate (or no-op head) policy.
type BackgroundCheck struct {
	Lifecycle *zk.ZoneLifecycle
	Source    *public.DNSZone
	Policy    *zonestate.Policy
	Reason    string
}

// BackgroundPolicy uses only tx reads and the guard's pinned immutable provider.
// It must validate the current source domain and captured policy identity.
type BackgroundPolicy func(context.Context, *sqlite.CommandTx, BackgroundCheck) error

// BackgroundGuard pins root's immutable credentials/policy through the FULL
// command, including writer lock wait and final commit/no-op check. It invokes
// fn synchronously exactly once with a nonnil current policy validator. No
// Store reentry or network inside fn. Nil guard disables ALL background writes.
type BackgroundGuard interface {
	WithCurrent(context.Context, pr.PublicationFence, func(BackgroundPolicy) error) error
}

// Options bounds complete membership/source scans and supplies current guards.
type Options struct {
	Store                *sqlite.Store
	Limits               zonestate.Limits
	MaxZones, MaxSources int
	MaxReadBytes         int64
	Credentials          CredentialGuard
	Background           BackgroundGuard
	Policy               PolicyGuard // optional additional check, never a replacement
}

// Adapter supplies the core's exact lifecycle/head callbacks.
type Adapter struct{ o Options }

// New requires actual SQLite with every private object carrier registered.
func New(o Options) (*Adapter, error) {
	if o.Store == nil {
		return nil, pr.ErrInvalid
	}
	if _, err := o.Store.ZoneStateBackend(o.Limits); err != nil {
		return nil, err
	}
	o.Limits = o.Limits.Defaults()
	if o.MaxZones <= 0 {
		o.MaxZones = 10000
	}
	if o.MaxSources <= 0 {
		o.MaxSources = 100000
	}
	if o.MaxReadBytes <= 0 {
		o.MaxReadBytes = 64 << 20
	}
	if o.MaxZones > 100000 || o.MaxSources > 1000000 || o.MaxReadBytes > 128<<20 {
		return nil, pr.ErrCapacity
	}
	return &Adapter{o: o}, nil
}

// LifecycleCAS validates exact object/source/HA and the complete prospective
// membership roster under the SAME transaction as the object mutation.
func (a *Adapter) LifecycleCAS(ctx context.Context, current, next *zk.ZoneLifecycle) (*zk.ZoneLifecycle, error) {
	if next == nil {
		return nil, pr.ErrInvalid
	}
	next = next.DeepCopy()
	current = current.DeepCopy()
	f, ok := pr.PublicationFenceFrom(ctx)
	if !ok || a.o.Background == nil {
		return nil, pr.ErrUnavailable
	}
	var saved *zk.ZoneLifecycle
	err := a.o.Background.WithCurrent(ctx, f, func(check BackgroundPolicy) error {
		if check == nil {
			return pr.ErrUnavailable
		}
		return a.o.Store.Command(ctx, f.Term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if !f.Term.Same(tx.Authority()) || f.Incarnation != tx.Incarnation() {
				return pr.ErrUnavailable
			}
			if next.Spec.Zone.ClusterID != tx.Authority().ClusterID {
				return pr.ErrInvalid
			}
			actual, err := readLifecycle(ctx, tx, next.Name)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			rv := ""
			if current != nil {
				rv = current.ResourceVersion
				if actual == nil || !sameLifecycle(actual, current) {
					return pr.ErrConflict
				}
			} else if actual != nil {
				return pr.ErrConflict
			}
			if err := zk.ValidateLifecycleTransition(actual, rv, next); err != nil {
				return pr.ErrConflict
			}
			z, err := source(ctx, tx, next)
			if err != nil {
				return err
			}
			if err := check(ctx, tx, BackgroundCheck{Lifecycle: next.DeepCopy(), Source: z, Reason: "lifecycle"}); err != nil {
				return err
			}
			roster, err := a.roster(ctx, tx)
			if err != nil {
				return err
			}
			if actual == nil && len(roster.ledgers) >= a.o.MaxZones {
				return pr.ErrCapacity
			}
			roster.ledgers[next.Name] = next
			if next.Spec.Phase == zk.PhaseRetired {
				v, err := a.readHead(ctx, tx, next.Spec.HeadZone())
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return err
				}
				if v.Head != nil && v.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
					return pr.ErrConflict
				}
			} else if next.Spec.Phase != zk.PhaseRetiring {
				if err := a.membership(ctx, tx, roster, next, nil, next.Spec.Phase == zk.PhaseActive); err != nil {
					return err
				}
			}
			if next.Spec.Phase == zk.PhaseActive && next.Spec.Type == public.ZoneTypePrimary {
				v, err := a.readHead(ctx, tx, next.Spec.HeadZone())
				if err != nil {
					return err
				}
				if v.Head.Spec.Lifecycle != zk.LifecycleActive {
					return pr.ErrConflict
				}
			}
			if err := a.witnesses(ctx, tx, roster, next); err != nil {
				return err
			}
			object, err := tx.Apply(ctx, next, store.ApplyOptions{Manager: "primaryruntime", ExpectedResourceVersion: rv, CreateOnly: current == nil})
			if err == nil {
				saved = object.(*zk.ZoneLifecycle)
			}
			return err
		})
	})
	if err != nil {
		return nil, err
	}
	return saved, nil
}

// Admission checks every response, including rejected/deduplicated/no-op UPDATE,
// inside a fenced transaction. Returned views are decoded in that exact tx;
// it never re-reads a possibly newer head after commit.
func (a *Adapter) Admission(ctx context.Context, c pr.Command) (*zonestate.View, error) {
	if c.Lifecycle == nil {
		return nil, pr.ErrInvalid
	}
	c.Lifecycle = c.Lifecycle.DeepCopy()
	c.Parent = c.Parent.DeepCopy()
	if c.Proof != nil {
		proof := *c.Proof
		c.Proof = &proof
	}
	if c.Prepared != nil {
		if _, err := zonestate.VerifyPrepared(c.Prepared, a.o.Limits); err != nil {
			return nil, err
		}
	}
	update := strings.HasPrefix(c.Reason, "update")
	if update && (c.Proof == nil || a.o.Credentials == nil) {
		return nil, pr.ErrUnavailable
	}
	if update {
		if c.Proof.NotAfter.IsZero() || !time.Now().Before(c.Proof.NotAfter) {
			return nil, pr.ErrUnavailable
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, c.Proof.NotAfter)
		defer cancel()
	}
	if !update && c.Proof != nil {
		return nil, pr.ErrInvalid
	}
	var out *zonestate.View
	f, hasFence := pr.PublicationFenceFrom(ctx)
	if !update && (!hasFence || a.o.Background == nil) {
		return nil, pr.ErrUnavailable
	}
	run := func(credential *CurrentCredential, check BackgroundPolicy) error {
		term := f.Term
		if !update && c.Expected.Zone.ClusterID != "" && !c.Expected.Term.Same(f.Term) {
			return pr.ErrUnavailable
		}
		if c.Proof != nil {
			term = c.Proof.Term
		}
		return a.o.Store.Command(ctx, term, func(ctx context.Context, tx *sqlite.CommandTx) error {
			if !update && (!f.Term.Same(tx.Authority()) || f.Incarnation != tx.Incarnation()) {
				return pr.ErrUnavailable
			}
			for _, l := range []*zk.ZoneLifecycle{c.Lifecycle, c.Parent} {
				if l == nil {
					continue
				}
				actual, err := readLifecycle(ctx, tx, l.Name)
				if err != nil {
					return err
				}
				if !sameLifecycle(actual, l) {
					return pr.ErrConflict
				}
				if l.Spec.Zone.ClusterID != tx.Authority().ClusterID {
					return pr.ErrInvalid
				}
				if _, err := source(ctx, tx, l); err != nil {
					return err
				}
			}
			target := c.Lifecycle
			if c.Parent != nil {
				target = c.Parent
			}
			if err := commandPhase(c); err != nil {
				return err
			}
			roster, err := a.roster(ctx, tx)
			if err != nil {
				return err
			}
			var except *zk.ZoneLifecycle
			if c.Parent != nil {
				except = c.Lifecycle
			}
			if target.Spec.Phase != zk.PhaseRetiring && target.Spec.Phase != zk.PhaseRetired {
				if err := a.membership(ctx, tx, roster, target, except, target.Spec.Phase == zk.PhaseActive); err != nil {
					return err
				}
			}
			if !update {
				z, err := source(ctx, tx, target)
				if err != nil {
					return err
				}
				var policy *zonestate.Policy
				if c.Prepared != nil {
					aggregate, err := zonestate.VerifyPrepared(c.Prepared, a.o.Limits)
					if err != nil {
						return err
					}
					policy = &aggregate.Policy
				} else if c.Expected.Zone.ClusterID != "" {
					v, err := a.readHead(ctx, tx, c.Expected.Zone)
					if err != nil {
						return err
					}
					policy = &v.Aggregate.Policy
				}
				if err := check(ctx, tx, BackgroundCheck{Lifecycle: target.DeepCopy(), Source: z, Policy: policy, Reason: c.Reason}); err != nil {
					return err
				}
			}
			if c.Expected.Zone.ClusterID == "" {
				if c.Prepared != nil || target.Spec.Type == public.ZoneTypePrimary || c.Reason != "replacement-membership" {
					return pr.ErrInvalid
				}
				return nil
			}
			if c.Expected.Zone != target.Spec.HeadZone() {
				return pr.ErrInvalid
			}
			v, err := a.readHead(ctx, tx, c.Expected.Zone)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if !sameExpected(v.Expected, c.Expected) {
				return pr.ErrConflict
			}
			if update {
				if v.Head == nil || v.Head.Spec.Lifecycle != zk.LifecycleActive || !c.Proof.Term.Same(tx.Authority()) || c.Proof.AuthorityIncarnation != tx.Incarnation() {
					return pr.ErrUnavailable
				}
				if err := checkCredential(*c.Proof, *credential, target); err != nil {
					return err
				}
				z, err := source(ctx, tx, target)
				if err != nil {
					return err
				}
				if err := credential.Policy(ctx, tx, z, v.Aggregate.Policy); err != nil {
					return err
				}
				if a.o.Policy != nil {
					if err := a.o.Policy(ctx, tx, z, v.Aggregate.Policy); err != nil {
						return err
					}
				}
				state, err := v.UpdateState()
				if err != nil {
					return err
				}
				empty := new(dns.Msg)
				empty.SetUpdate(state.Apex)
				auth, pe := dynamicupdate.Parse(empty, dynamicupdate.Limits{})
				if pe != nil {
					return pe
				}
				if _, pe := dynamicupdate.Plan(state, auth, c.Proof.Principal, time.Now(), dynamicupdate.Limits{}); pe != nil {
					return pe
				}
			}
			if c.Reason == "source-reconcile" {
				if err := a.desired(ctx, tx, c, target); err != nil {
					return err
				}
			}
			if c.Prepared == nil {
				if v.Head == nil {
					return pr.ErrConflict
				}
				out = v
				return nil
			}
			candidate, err := zonestate.VerifyPrepared(c.Prepared, a.o.Limits)
			if err != nil {
				return err
			}
			if err := a.candidate(ctx, tx, roster, c, v, candidate); err != nil {
				return err
			}
			head, err := tx.CommitZoneState(ctx, c.Expected, c.Prepared, a.o.Limits, nil)
			if err != nil {
				return err
			}
			out, err = a.decode(ctx, tx, head)
			return err
		})
	}
	var err error
	if update {
		err = a.o.Credentials.WithCurrent(ctx, *c.Proof, func(current CurrentCredential) error {
			if current.Policy == nil {
				return pr.ErrUnavailable
			}
			return run(&current, nil)
		})
	} else {
		err = a.o.Background.WithCurrent(ctx, f, func(check BackgroundPolicy) error {
			if check == nil {
				return pr.ErrUnavailable
			}
			return run(nil, check)
		})
	}
	if err != nil {
		return nil, err
	}
	return out, nil
}
func sameLifecycle(a, b *zk.ZoneLifecycle) bool {
	return a != nil && b != nil && a.Validate() == nil && b.Validate() == nil && a.Name == b.Name && a.UID == b.UID && a.ResourceVersion != "" && a.ResourceVersion == b.ResourceVersion && reflect.DeepEqual(a.Spec, b.Spec)
}
func sameExpected(a, b zonestate.Expected) bool {
	return a.Zone == b.Zone && a.ResourceVersion == b.ResourceVersion && a.CommitSequence == b.CommitSequence && a.RootDigest == b.RootDigest && a.AuthEpoch == b.AuthEpoch && a.Term.Same(b.Term)
}
func checkCredential(p pr.ProofIdentity, current CurrentCredential, l *zk.ZoneLifecycle) error {
	if p.Principal != current.Principal || p.ProviderVersion != current.ProviderVersion || p.Algorithm != current.Algorithm || p.Domain != "dns-update:"+zk.HeadName(l.Spec.Zone) || p.Subject != p.Principal.Key {
		return pr.ErrUnavailable
	}
	return nil
}
func commandPhase(c pr.Command) error {
	l := c.Lifecycle
	switch c.Reason {
	case "source-reconcile":
		if c.Parent != nil || l.Spec.Type != public.ZoneTypePrimary || (l.Spec.Phase != zk.PhaseFencing && l.Spec.Phase != zk.PhaseActive) {
			return pr.ErrInvalid
		}
	case "parent-fence", "late-parent-fence":
		if c.Parent == nil || (l.Spec.Phase != zk.PhasePreparing && l.Spec.Phase != zk.PhaseFencing && l.Spec.Phase != zk.PhaseActive) {
			return pr.ErrInvalid
		}
	case "parent-release":
		if c.Parent == nil || l.Spec.Phase != zk.PhaseRetired {
			return pr.ErrInvalid
		}
	case "tombstone":
		if c.Parent != nil || l.Spec.Phase != zk.PhaseRetiring || c.Prepared == nil || c.Prepared.Head.Spec.Lifecycle != zk.LifecycleTombstoned {
			return pr.ErrInvalid
		}
	case "replacement-membership":
		if l.Spec.Phase != zk.PhaseActive || l.Spec.Type == public.ZoneTypePrimary || c.Parent != nil {
			return pr.ErrInvalid
		}
	case "notify-delivery", "notify-ack", "update", "update-dedup", "update-rejected", "update-capacity", "update-publication-recovery":
		if c.Parent != nil || l.Spec.Phase != zk.PhaseActive || l.Spec.Type != public.ZoneTypePrimary {
			return pr.ErrUnavailable
		}
	default:
		return pr.ErrInvalid
	}
	if c.Parent != nil && (c.Parent.Spec.Type != public.ZoneTypePrimary || (c.Parent.Spec.Phase != zk.PhaseActive && c.Parent.Spec.Phase != zk.PhaseFencing) || !dns.IsSubDomain(c.Parent.Spec.Zone.Apex, l.Spec.Zone.Apex) || c.Parent.Spec.Zone.Apex == l.Spec.Zone.Apex) {
		return pr.ErrInvalid
	}
	return nil
}
