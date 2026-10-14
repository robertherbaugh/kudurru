// Package currentguard binds primary admission to a root-pinned immutable
// credential generation. It owns no serving pointer, HA authority or network.
package currentguard

import (
	"context"
	"sync/atomic"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	zk "github.com/robertherbaugh/kudurru/api/zonekinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime/sqliteadapter"
	"github.com/robertherbaugh/kudurru/internal/primaryzone"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/zonestate"
)

// Pinned is one root-owned CURRENT immutable dependency generation. Keys must
// be nonnil, including an explicit empty ring. Notify is an optional validator
// bound to this same pin; nil disables NOTIFY delivery/ack admission. It cannot
// attest a future sender's credentials: root also owns the immutable sender.
type Pinned struct {
	Identity string
	Keys     *dnsserver.StaticKeyring
	Notify   sqliteadapter.BackgroundPolicy
	// Current is a mandatory memory-only predicate for the exact retained
	// dependency cut, including its absolute deadlines. It must take no locks
	// or perform I/O; replacement exclusion alone does not stop time expiry.
	Current func() bool
}

// PinFunc holds real dependency-replacement/publication exclusion through ALL
// of fn, including Store.Command's writer-lock wait and final commit/no-op fence.
// Invoke fn exactly once synchronously and return its error unchanged. Take no
// Store/network action inside the pin implementation. Lock order is publication
// -> dependency replacement -> SQLite. Release before publication waits/sends.
// Recovery must permit a current dependency pin while serving is pending; an
// obsolete served ring or preflight authority check is not current admission.
type PinFunc func(context.Context, pr.PublicationFence, func(Pinned) error) error

// Options bounds policy compilation. Nil Pin makes every operation unavailable.
type Options struct {
	Pin    PinFunc
	Limits zonestate.Limits
}

// Owner shares one compiler and pin source between reader and admission guards.
type Owner struct{ options Options }

// New creates an owner without I/O or constructing a TSIG verification proof.
func New(o Options) (*Owner, error) {
	o.Limits = o.Limits.Defaults()
	return &Owner{options: o}, nil
}

type credentialGuard struct{ owner *Owner }
type backgroundGuard struct{ owner *Owner }

// Credentials returns the mandatory same-generation UPDATE guard.
func (o *Owner) Credentials() sqliteadapter.CredentialGuard { return credentialGuard{o} }

// Background returns the lifecycle/reconcile/notification guard.
func (o *Owner) Background() sqliteadapter.BackgroundGuard { return backgroundGuard{o} }

func (o *Owner) pinned(ctx context.Context, f pr.PublicationFence, fn func(Pinned, *atomic.Bool) error) error {
	if o == nil || o.options.Pin == nil || fn == nil || f.Term.Validate() != nil || f.Incarnation == 0 {
		return pr.ErrUnavailable
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var calls atomic.Uint32
	var callbackErr error
	err := o.options.Pin(ctx, f, func(p Pinned) error {
		if calls.Add(1) != 1 {
			return pr.ErrUnavailable
		}
		active := new(atomic.Bool)
		active.Store(true)
		defer active.Store(false)
		if p.Keys == nil || p.Identity == "" || len(p.Identity) > 512 || p.Current == nil || !p.Current() {
			callbackErr = pr.ErrUnavailable
		} else if callbackErr = ctx.Err(); callbackErr == nil {
			callbackErr = fn(p, active)
		}
		return callbackErr
	})
	if calls.Load() != 1 {
		return pr.ErrUnavailable
	}
	if callbackErr != nil {
		return callbackErr
	}
	return err
}

func (g credentialGuard) WithCurrent(ctx context.Context, proof pr.ProofIdentity, fn func(sqliteadapter.CurrentCredential) error) error {
	if fn == nil || proof.NotAfter.IsZero() || !time.Now().Before(proof.NotAfter) || proof.Subject != proof.Principal.Key || (proof.Transport != "udp" && proof.Transport != "tcp" && proof.Transport != "dot") {
		return pr.ErrUnavailable
	}
	ctx, cancel := context.WithDeadline(ctx, proof.NotAfter)
	defer cancel()
	f := pr.PublicationFence{Term: proof.Term, Incarnation: proof.AuthorityIncarnation}
	return g.owner.pinned(ctx, f, func(p Pinned, active *atomic.Bool) error {
		id, ok := p.Keys.Identity(proof.Principal.Key)
		if !ok || proof.Principal != (dynamicupdate.Principal{Key: id.Key, Version: id.CredentialVersion}) || proof.ProviderVersion != id.ProviderVersion || proof.Algorithm != id.Algorithm {
			return pr.ErrUnavailable
		}
		return fn(sqliteadapter.CurrentCredential{
			Principal: proof.Principal, ProviderVersion: id.ProviderVersion, Algorithm: id.Algorithm,
			Policy: func(commandCtx context.Context, tx *sqlite.CommandTx, source *public.DNSZone, policy zonestate.Policy) error {
				if err := admitted(ctx, commandCtx, active, p, f, tx); err != nil {
					return err
				}
				if !time.Now().Before(proof.NotAfter) {
					return pr.ErrUnavailable
				}
				if err := g.owner.checkPolicy(commandCtx, p.Keys, source, policy); err != nil {
					return err
				}
				return tx.RequireCommitCheck(func(finalCtx context.Context) error {
					if !time.Now().Before(proof.NotAfter) {
						return pr.ErrUnavailable
					}
					return stillPinned(ctx, finalCtx, active, p)
				})
			},
		})
	})
}

func (g backgroundGuard) WithCurrent(ctx context.Context, f pr.PublicationFence, fn func(sqliteadapter.BackgroundPolicy) error) error {
	if fn == nil {
		return pr.ErrUnavailable
	}
	return g.owner.pinned(ctx, f, func(p Pinned, active *atomic.Bool) error {
		return fn(func(commandCtx context.Context, tx *sqlite.CommandTx, check sqliteadapter.BackgroundCheck) error {
			if err := admitted(ctx, commandCtx, active, p, f, tx); err != nil {
				return err
			}
			if err := g.owner.checkBackground(commandCtx, tx, p, check); err != nil {
				return err
			}
			return tx.RequireCommitCheck(func(finalCtx context.Context) error {
				return stillPinned(ctx, finalCtx, active, p)
			})
		})
	})
}

func admitted(pinCtx, ctx context.Context, active *atomic.Bool, p Pinned, f pr.PublicationFence, tx *sqlite.CommandTx) error {
	if !active.Load() || tx == nil || !f.Term.Same(tx.Authority()) || f.Incarnation != tx.Incarnation() {
		return pr.ErrUnavailable
	}
	return stillPinned(pinCtx, ctx, active, p)
}

func stillPinned(pinCtx, ctx context.Context, active *atomic.Bool, p Pinned) error {
	if !active.Load() {
		return pr.ErrUnavailable
	}
	if err := pinCtx.Err(); err != nil {
		return err
	}
	if p.Current == nil || !p.Current() {
		return pr.ErrUnavailable
	}
	return ctx.Err()
}

// CapturePolicy captures metadata outside the source-reader transaction. The
// commit validator recompiles against its exact tx source and pinned generation.
func (o *Owner) CapturePolicy(ctx context.Context, z *public.DNSZone) (*zonestate.Policy, error) {
	f, ok := pr.PublicationFenceFrom(ctx)
	if !ok {
		return nil, pr.ErrUnavailable
	}
	var result *zonestate.Policy
	err := o.pinned(ctx, f, func(p Pinned, _ *atomic.Bool) error {
		var err error
		result, err = o.compile(ctx, p.Keys, z)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (o *Owner) checkBackground(ctx context.Context, tx *sqlite.CommandTx, p Pinned, c sqliteadapter.BackgroundCheck) error {
	l := c.Lifecycle
	if l == nil || l.Validate() != nil || l.Spec.Zone.ClusterID != tx.Authority().ClusterID {
		return pr.ErrUnavailable
	}
	retiring := l.Spec.Phase == zk.PhaseRetiring || l.Spec.Phase == zk.PhaseRetired
	if c.Source == nil {
		if !retiring || !l.Spec.Source.Removed {
			return pr.ErrUnavailable
		}
	} else {
		token, err := primaryzone.SourceToken(c.Source)
		if err != nil {
			return err
		}
		if c.Source.Name != l.Spec.Zone.ObjectName || token.UID != l.Spec.Source.UID || token.ResourceVersion != l.Spec.Source.ResourceVersion || token.IntentDigest != l.Spec.Source.IntentDigest || l.Spec.Source.Removed {
			return pr.ErrConflict
		}
		if !retiring && (string(c.Source.UID) != l.Spec.Zone.UID || c.Source.Spec.Type != l.Spec.Type || c.Source.Spec.ZoneName != l.Spec.Zone.Apex) {
			apex, err := dynamicupdate.CanonicalName(c.Source.Spec.ZoneName)
			if err != nil || string(c.Source.UID) != l.Spec.Zone.UID || c.Source.Spec.Type != l.Spec.Type || apex != l.Spec.Zone.Apex {
				return pr.ErrConflict
			}
		}
	}
	switch c.Reason {
	case "lifecycle":
		if c.Policy != nil {
			return pr.ErrUnavailable
		}
		return nil
	case "source-reconcile":
		if retiring || l.Spec.Type != public.ZoneTypePrimary || c.Source == nil || c.Policy == nil {
			return pr.ErrUnavailable
		}
		return o.checkPolicy(ctx, p.Keys, c.Source, *c.Policy)
	case "replacement-membership":
		if l.Spec.Phase != zk.PhaseActive || l.Spec.Type == public.ZoneTypePrimary || c.Policy != nil {
			return pr.ErrUnavailable
		}
		return nil
	case "parent-fence", "late-parent-fence", "parent-release":
		if c.Source == nil || l.Spec.Type != public.ZoneTypePrimary || (l.Spec.Phase != zk.PhaseActive && l.Spec.Phase != zk.PhaseFencing) || c.Policy == nil || c.Policy.AuthEpoch == 0 {
			return pr.ErrUnavailable
		}
		// Adapter independently permits only the child's system reservation
		// change. Retained UPDATE credentials need not authorize boundary repair.
		return nil
	case "tombstone":
		if l.Spec.Phase != zk.PhaseRetiring || c.Policy == nil || c.Policy.AuthEpoch == 0 {
			return pr.ErrUnavailable
		}
		return nil
	case "notify-delivery", "notify-ack":
		if p.Notify == nil || c.Source == nil || l.Spec.Type != public.ZoneTypePrimary || l.Spec.Phase != zk.PhaseActive || c.Policy == nil {
			return pr.ErrUnavailable
		}
		return p.Notify(ctx, tx, c)
	default:
		return pr.ErrUnavailable
	}
}
