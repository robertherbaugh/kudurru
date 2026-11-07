package replica

import (
	"context"
	"encoding/json"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// CommandFormat identifies the versioned write command envelope.
const CommandFormat = "kudurru.replica.command"

// SubjectRef carries identity/credential references, never peer-selected rights.
// The primary owner must verify current credential validity and reload grants.
type SubjectRef struct {
	Kind           auth.PrincipalKind `json:"kind"`
	ID             string             `json:"id"`
	Provider       string             `json:"provider"`
	CredentialKind string             `json:"credentialKind"`
	CredentialID   string             `json:"credentialID"`
	IssuedAt       time.Time          `json:"issuedAt"`
}

// Precondition names a current object version the domain must check in its tx.
type Precondition struct {
	Kind            store.Kind `json:"kind"`
	Namespace       string     `json:"namespace,omitempty"`
	Name            string     `json:"name"`
	ResourceVersion string     `json:"resourceVersion"`
}

// Command carries one immutable request identity with a replaceable term fence.
type Command struct {
	Format        string           `json:"format"`
	Version       int              `json:"version"`
	ID            string           `json:"id"`
	ExpectedTerm  replication.Term `json:"expectedTerm"`
	Hops          int              `json:"hops"`
	IssuedAt      time.Time        `json:"issuedAt"`
	NotAfter      time.Time        `json:"notAfter"`
	Operation     string           `json:"operation"`
	Subject       SubjectRef       `json:"subject"`
	Preconditions []Precondition   `json:"preconditions"`
	Payload       json.RawMessage  `json:"payload"`
}

// Outcome contains the domain's bounded JSON result and local secrecy assertion.
type Outcome struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body"`
	// Sensitive is an owner assertion, intentionally absent from the wire.
	Sensitive bool `json:"-"`
}

// DedupRecord persists the immutable fingerprint, expiry and owner-sealed result.
type DedupRecord struct {
	Version     int             `json:"version"`
	Fingerprint string          `json:"fingerprint"`
	ExpiresAt   time.Time       `json:"expiresAt"`
	Result      json.RawMessage `json:"result"`
}

func (o Outcome) validate(limit int64) error {
	if o.Status < 200 || o.Status > 599 || len(o.Body) == 0 {
		return ErrProtocol
	}
	if int64(len(o.Body)) > limit {
		return replication.ErrTooLarge
	}
	if _, err := canonicalJSON(o.Body); err != nil {
		return err
	}
	wire, err := json.Marshal(o)
	if err != nil {
		return err
	}
	if int64(len(wire)) > limit {
		return replication.ErrTooLarge
	}
	return nil
}

// AtomicCommands checks expected authority/writer incarnation under the writer
// lock. Callback current reads, domain mutations and dedup share ONE transaction.
// A callback/commit error rolls all of them back; no independently committed
// Store.Apply is allowed in the callback. Production adapter is SQLiteAtomic.
type AtomicCommands interface {
	Transact(context.Context, replication.Term, func(context.Context, CommandTx) error) error
}

// CommandTx joins tx-bound domain state and dedup storage in one commit.
type CommandTx interface {
	State() any
	GetDedup(context.Context, string) (DedupRecord, bool, error)
	PutDedup(context.Context, string, DedupRecord) error
}

// Domain resolves current credentials/rights and executes typed mutations in tx.
type Domain interface {
	Authorize(context.Context, CommandTx, Command) (context.Context, error)
	Execute(context.Context, CommandTx, Command) (Outcome, error)
}

// ResultCodec must seal sensitive dedup outcomes with the IAM owner's keyring.
type ResultCodec interface {
	Seal(context.Context, Outcome) (json.RawMessage, error)
	Open(context.Context, json.RawMessage) (Outcome, error)
}
type plainCodec struct{}

func (plainCodec) Seal(_ context.Context, o Outcome) (json.RawMessage, error) {
	if o.Sensitive {
		return nil, ErrSensitive
	}
	b, err := json.Marshal(o)
	return b, err
}
func (plainCodec) Open(_ context.Context, b json.RawMessage) (Outcome, error) {
	var o Outcome
	err := decodeStrict(b, &o)
	return o, err
}

// CommandService serializes current authorization, execution and durable dedup.
type CommandService struct {
	Backend        AtomicCommands
	Domain         Domain
	Codec          ResultCodec
	Now            func() time.Time
	MaxLifetime    time.Duration
	MaxResultBytes int64
}

func (c Command) validate(now time.Time, maxLifetime time.Duration) error {
	if c.Format != CommandFormat || c.Version != 1 || !validID(c.ID) || !validID(c.Operation) || c.Subject.ID == "" || len(c.Subject.ID) > 1024 || c.Subject.Provider == "" || len(c.Subject.Provider) > 256 || len(c.Subject.CredentialID) > 1024 || c.Subject.CredentialID == "" || c.Subject.IssuedAt.IsZero() || len(c.Preconditions) > 10000 {
		return ErrProtocol
	}
	if c.Hops != 1 {
		return ErrLoop
	}
	switch c.Subject.Kind {
	case auth.KindUser, auth.KindServiceAccount, auth.KindKubernetesServiceAccount:
	default:
		return ErrForbidden
	}
	switch c.Subject.CredentialKind {
	case "session", "api-token", "oidc", "kubernetes":
	default:
		return ErrProtocol
	}
	if err := c.ExpectedTerm.Validate(); err != nil {
		return err
	}
	if c.IssuedAt.IsZero() || c.NotAfter.IsZero() || !c.NotAfter.After(c.IssuedAt) || c.NotAfter.Sub(c.IssuedAt) > maxLifetime || c.IssuedAt.After(now.Add(30*time.Second)) {
		return ErrProtocol
	}
	if !now.Before(c.NotAfter) {
		return ErrExpired
	}
	if len(c.Payload) == 0 {
		return ErrProtocol
	}
	if _, err := canonicalJSON(c.Payload); err != nil {
		return err
	}
	seen := map[store.Key]bool{}
	for _, p := range c.Preconditions {
		k := store.Key{Kind: p.Kind, Namespace: p.Namespace, Name: p.Name}
		if p.Kind == "" || p.Name == "" || seen[k] {
			return ErrProtocol
		}
		seen[k] = true
	}
	return nil
}
func (c Command) fingerprint() (string, error) {
	// ExpectedTerm is an execution fence, not command identity. This lets an
	// inherited dedup outcome answer the exact same ID after promotion.
	c.ID = ""
	c.ExpectedTerm = replication.Term{}
	c.Hops = 0
	b, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	b, err = canonicalJSON(b)
	if err != nil {
		return "", err
	}
	return fingerprint(b), nil
}

// Execute authorizes even retained outcomes and rolls back every failed command.
func (s *CommandService) Execute(ctx context.Context, c Command) (Outcome, error) {
	if s == nil || s.Backend == nil || s.Domain == nil {
		return Outcome{}, ErrUnavailable
	}
	clock := time.Now
	if s.Now != nil {
		clock = s.Now
	}
	now := clock()
	life := s.MaxLifetime
	if life <= 0 {
		life = DefaultCommandLifetime
	}
	if err := c.validate(now, life); err != nil {
		return Outcome{}, err
	}
	hash, err := c.fingerprint()
	if err != nil {
		return Outcome{}, err
	}
	codec := s.Codec
	if codec == nil {
		codec = plainCodec{}
	}
	limit := s.MaxResultBytes
	if limit <= 0 {
		limit = DefaultMaxResultBytes
	}
	var out Outcome
	err = s.Backend.Transact(ctx, c.ExpectedTerm, func(txCtx context.Context, tx CommandTx) (err error) {
		// The writer lock may have been contended since the outer validation.
		now := clock()
		if err := c.validate(now, life); err != nil {
			return err
		}
		defer func() {
			if err == nil && !clock().Before(c.NotAfter) {
				err = ErrExpired
			}
		}()
		authorized, err := s.Domain.Authorize(txCtx, tx, c)
		if err != nil {
			return err
		}
		if authorized == nil {
			return ErrForbidden
		}
		previous, ok, err := tx.GetDedup(authorized, c.ID)
		if err != nil {
			return err
		}
		if ok {
			if previous.Version != 1 || previous.Fingerprint != hash || !previous.ExpiresAt.Equal(c.NotAfter) {
				return ErrDedupConflict
			}
			if !now.Before(previous.ExpiresAt) {
				return ErrExpired
			}
			if len(previous.Result) == 0 || int64(len(previous.Result)) > limit {
				return replication.ErrTooLarge
			}
			out, err = codec.Open(authorized, previous.Result)
			if err != nil {
				return err
			}
			return out.validate(limit)
		}
		out, err = s.Domain.Execute(authorized, tx, c)
		if err != nil {
			return err
		}
		if err := out.validate(limit); err != nil {
			return err
		}
		encoded, err := codec.Seal(authorized, out)
		if err != nil {
			return err
		}
		if int64(len(encoded)) > limit || len(encoded) == 0 {
			return replication.ErrTooLarge
		}
		return tx.PutDedup(authorized, c.ID, DedupRecord{Version: 1, Fingerprint: hash, ExpiresAt: c.NotAfter, Result: encoded})
	})
	if err != nil {
		return Outcome{}, err
	}
	return out, nil
}

type forwardedKey struct{}

func withForwarded(ctx context.Context) context.Context {
	return context.WithValue(ctx, forwardedKey{}, true)
}

// IsForwarded reports a primary-side command context that must not be forwarded.
func IsForwarded(ctx context.Context) bool { v, _ := ctx.Value(forwardedKey{}).(bool); return v }

// Mutation exposes store arguments to the root-owned domain command encoder.
type Mutation struct {
	Operation     string
	Object        store.Object
	Key           store.Key
	ApplyOptions  store.ApplyOptions
	DeleteOptions store.DeleteOptions
}

// CommandEncoder derives a trusted subject, lifetime and domain preconditions.
type CommandEncoder func(context.Context, replication.Term, Mutation) (Command, error)

// Forwarder delegates operation-specific identity/precondition encoding to root.
// Compound API operations should use Client.Write BEFORE follower-local reads.
type Forwarder struct {
	Client  *Client
	Resolve PeerResolver
	Term    func() replication.Term
	Encode  CommandEncoder
}

var _ interface {
	Apply(context.Context, store.Object, store.ApplyOptions) (store.Object, error)
	Delete(context.Context, store.Key, store.DeleteOptions) error
	UpdateStatus(context.Context, store.Object) error
} = (*Forwarder)(nil)

func (f *Forwarder) forward(ctx context.Context, m Mutation) (Outcome, error) {
	if IsForwarded(ctx) || sqlite.HasWriterToken(ctx) {
		return Outcome{}, ErrLoop
	}
	if f == nil || f.Client == nil || f.Resolve == nil || f.Term == nil || f.Encode == nil {
		return Outcome{}, ErrUnavailable
	}
	peer, err := f.Resolve(ctx)
	if err != nil {
		return Outcome{}, err
	}
	c, err := f.Encode(ctx, f.Term(), m)
	if err != nil {
		return Outcome{}, err
	}
	return f.Client.Write(ctx, peer, c)
}

// Apply forwards a domain command and decodes its object result.
func (f *Forwarder) Apply(ctx context.Context, o store.Object, opt store.ApplyOptions) (store.Object, error) {
	if o == nil {
		return nil, ErrProtocol
	}
	result, err := f.forward(ctx, Mutation{Operation: "apply", Object: o, ApplyOptions: opt})
	if err != nil {
		return nil, err
	}
	if result.Status >= 400 {
		return nil, ErrProtocol
	}
	out := o.DeepCopyObject().(store.Object)
	if err := json.Unmarshal(result.Body, out); err != nil {
		return nil, ErrProtocol
	}
	return out, nil
}

// Delete forwards a domain command without mutating the local mirror.
func (f *Forwarder) Delete(ctx context.Context, k store.Key, opt store.DeleteOptions) error {
	out, err := f.forward(ctx, Mutation{Operation: "delete", Key: k, DeleteOptions: opt})
	if err != nil {
		return err
	}
	if out.Status >= 400 {
		return ErrProtocol
	}
	return nil
}

// UpdateStatus forwards public domain work; controller-token contexts are denied.
func (f *Forwarder) UpdateStatus(ctx context.Context, o store.Object) error {
	if o == nil {
		return ErrProtocol
	}
	out, err := f.forward(ctx, Mutation{Operation: "update-status", Object: o})
	if err != nil {
		return err
	}
	if out.Status >= 400 {
		return ErrProtocol
	}
	return nil
}
