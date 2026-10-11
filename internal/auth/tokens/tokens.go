// Package tokens implements the API tokens and service accounts of
// docs/architecture.md §8.6: tokens in the wire format
// kdr_<12-char id>_<43-char secret>, stored as APIToken objects holding
// only hex(sha256(token)), compared in constant time on the full hash with
// the same 401 for an unknown id and a wrong secret (no Technitium-style
// prefix oracle), optional expiry capped by auth.tokens.maxLifetime,
// lastUsedAt/IP, revocation, personal tokens that act as their owner, and
// service-account tokens including the KUDURRU_BOOTSTRAP_TOKEN one.
package tokens

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/objcache"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Wire format constants.
const (
	Prefix    = "kdr_"
	IDLen     = 12
	SecretLen = 43 // base64url of 32 bytes, unpadded
	// WireLen is len("kdr_") + 12 + len("_") + 43.
	WireLen = len(Prefix) + IDLen + 1 + SecretLen
)

// Errors of Create and Revoke.
var (
	ErrNotFound         = errors.New("tokens: no such token")
	ErrLifetimeTooLong  = errors.New("tokens: expiry exceeds auth.tokens.maxLifetime")
	ErrInvalidOwner     = errors.New("tokens: invalid owner")
	ErrNoServiceAccount = errors.New("tokens: no such service account")
)

// Generate returns a fresh token in wire form and its id.
func Generate() (wire, id string) {
	id = strings.ToLower(rand.Text())[:IDLen] // base32: [a-z2-7]
	secret := make([]byte, 32)
	_, _ = rand.Read(secret)
	return Prefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret), id
}

// Parse returns the id of a well-formed wire token. The secret may itself
// contain '_' (base64url), so the id is located by position.
func Parse(wire string) (id string, ok bool) {
	if len(wire) != WireLen || !strings.HasPrefix(wire, Prefix) || wire[len(Prefix)+IDLen] != '_' {
		return "", false
	}
	id = wire[len(Prefix) : len(Prefix)+IDLen]
	for _, c := range id {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return "", false
		}
	}
	for _, c := range wire[len(Prefix)+IDLen+1:] {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' {
			return "", false
		}
	}
	return id, true
}

// Hash returns hex(sha256(wire)), the only form a token is stored in.
func Hash(wire string) string {
	sum := sha256.Sum256([]byte(wire))
	return hex.EncodeToString(sum[:])
}

// dummyHash is compared when the id is unknown, so both failures cost the
// same.
var dummyHash = strings.Repeat("0", 64)

// Options configures Tokens.
type Options struct {
	// Store persists APIToken and ServiceAccount objects. Required.
	Store store.Store
	// MaxLifetime caps expiry (auth.tokens.maxLifetime); 0 allows tokens
	// that never expire.
	MaxLifetime time.Duration
	// Namespace holds kudurru service accounts created by kudurru itself
	// (bootstrap, break-glass); default "kudurru-system".
	Namespace string
	// Logger; default discards.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// DefaultNamespace is the namespace of kudurru's own service accounts.
const DefaultNamespace = "kudurru-system"

// lastUsedInterval throttles lastUsedAt writes per token.
const lastUsedInterval = time.Minute

// Tokens is the token store and the bearer kdr_ step of the chain.
type Tokens struct {
	st   store.Store
	opts Options
	toks *objcache.Cache[*internalv1.APIToken]
	sas  *objcache.Cache[*internalv1.ServiceAccount]
	log  *slog.Logger

	usedMu sync.Mutex
	used   map[string]time.Time
	usedCh chan usage
}

type usage struct {
	id, ip string
	at     time.Time
}

// New builds the store; call Prime before serving and Run to keep the
// caches current and record lastUsedAt.
func New(opts Options) (*Tokens, error) {
	if opts.Store == nil {
		return nil, errors.New("tokens: Options.Store is required")
	}
	if opts.MaxLifetime < 0 {
		return nil, errors.New("tokens: negative MaxLifetime")
	}
	if opts.Namespace == "" {
		opts.Namespace = DefaultNamespace
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Tokens{
		st: opts.Store, opts: opts, log: opts.Logger,
		toks:   objcache.New[*internalv1.APIToken](opts.Store, internalv1.KindAPIToken, objcache.Options{Logger: opts.Logger}),
		sas:    objcache.New[*internalv1.ServiceAccount](opts.Store, internalv1.KindServiceAccount, objcache.Options{Logger: opts.Logger}),
		used:   map[string]time.Time{},
		usedCh: make(chan usage, 256),
	}, nil
}

// Namespace returns the namespace of kudurru's own service accounts.
func (t *Tokens) Namespace() string { return t.opts.Namespace }

// ServiceAccounts returns the service-account cache.
func (t *Tokens) ServiceAccounts() *objcache.Cache[*internalv1.ServiceAccount] { return t.sas }

// Prime loads tokens and service accounts.
func (t *Tokens) Prime(ctx context.Context) error {
	if err := t.toks.Prime(ctx); err != nil {
		return err
	}
	return t.sas.Prime(ctx)
}

// Run keeps both caches current and writes lastUsedAt/IP (at most once a
// minute per token) until ctx is done.
func (t *Tokens) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Go(func() { _ = t.toks.Run(ctx) })
	wg.Go(func() { _ = t.sas.Run(ctx) })
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return nil
		case u := <-t.usedCh:
			t.recordUse(ctx, u)
		}
	}
}

func (t *Tokens) recordUse(ctx context.Context, u usage) {
	var cur internalv1.APIToken
	if err := t.st.Get(ctx, store.Key{Kind: internalv1.KindAPIToken, Name: u.id}, &cur); err != nil {
		return
	}
	at := metav1.NewTime(u.at.UTC().Truncate(time.Second))
	cur.Status.LastUsedAt, cur.Status.LastUsedIP = &at, u.ip
	cur.ResourceVersion = ""
	if err := t.st.UpdateStatus(ctx, &cur); err != nil {
		t.log.DebugContext(ctx, "recording token use failed", "token", u.id, "err", err)
	}
}

// Get returns a cached token by id (shared object).
func (t *Tokens) Get(id string) (*internalv1.APIToken, bool) {
	return t.toks.Get(store.Key{Kind: internalv1.KindAPIToken, Name: id})
}

// List returns the tokens whose owner ID is ownerID ("" for all), newest
// first.
func (t *Tokens) List(ownerID string) []*internalv1.APIToken {
	var out []*internalv1.APIToken
	for _, tok := range t.toks.List() {
		if ownerID == "" || tok.Spec.Owner.ID == ownerID {
			out = append(out, tok)
		}
	}
	slices.SortFunc(out, func(a, b *internalv1.APIToken) int {
		if c := b.CreationTimestamp.Compare(a.CreationTimestamp.Time); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// CreateOptions describes a new token.
type CreateOptions struct {
	// Owner is who the token acts as. A User owner needs Name and ID; a
	// ServiceAccount owner needs Name and Namespace of an existing account.
	Owner internalv1.TokenOwner
	// Description is a human label.
	Description string
	// ExpiresIn is the lifetime; 0 means MaxLifetime (or never when it is
	// 0). Longer than MaxLifetime is ErrLifetimeTooLong.
	ExpiresIn time.Duration
	// Grants are frozen grants for owners whose bindings cannot be
	// re-resolved per request (OIDC users: the session's roles snapshot).
	Grants []string
	// Scopes narrow the token (grants in wire form).
	Scopes []string
}

// Create stores a new token and returns its wire form, shown once.
func (t *Tokens) Create(ctx context.Context, o CreateOptions) (string, *internalv1.APIToken, error) {
	if o.ExpiresIn < 0 {
		return "", nil, errors.New("tokens: negative expiry")
	}
	switch o.Owner.Kind {
	case internalv1.OwnerUser:
		if o.Owner.Name == "" || o.Owner.ID == "" {
			return "", nil, fmt.Errorf("%w: a user owner needs name and id", ErrInvalidOwner)
		}
	case internalv1.OwnerServiceAccount:
		if _, ok := t.serviceAccount(o.Owner.Namespace, o.Owner.Name); !ok {
			var sa internalv1.ServiceAccount
			if err := t.st.Get(ctx, saKey(o.Owner.Namespace, o.Owner.Name), &sa); err != nil {
				return "", nil, fmt.Errorf("%w: %s/%s", ErrNoServiceAccount, o.Owner.Namespace, o.Owner.Name)
			}
		}
		o.Owner.ID = ServiceAccountID(o.Owner.Namespace, o.Owner.Name)
	default:
		return "", nil, fmt.Errorf("%w: kind %q", ErrInvalidOwner, o.Owner.Kind)
	}
	life := o.ExpiresIn
	if m := t.opts.MaxLifetime; m > 0 {
		if life == 0 {
			life = m
		} else if life > m {
			return "", nil, fmt.Errorf("%w (%s > %s)", ErrLifetimeTooLong, life, m)
		}
	}
	wire, id := Generate()
	tok := &internalv1.APIToken{ObjectMeta: metav1.ObjectMeta{Name: id}, Spec: internalv1.APITokenSpec{
		Owner: o.Owner, Description: o.Description, SecretHash: Hash(wire),
		Grants: slices.Clone(o.Grants), Scopes: slices.Clone(o.Scopes),
	}}
	if life > 0 {
		exp := metav1.NewTime(t.opts.Now().Add(life).UTC().Truncate(time.Second))
		tok.Spec.ExpiresAt = &exp
	}
	if err := tok.Validate(); err != nil {
		return "", nil, err
	}
	out, err := t.st.Apply(ctx, tok, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		return "", nil, err
	}
	created := out.(*internalv1.APIToken)
	t.toks.Upsert(created)
	t.log.InfoContext(ctx, "api token created", "token", id, "owner", o.Owner.ID, "expiresAt", tok.Spec.ExpiresAt)
	return wire, created, nil
}

// Revoke deletes a token.
func (t *Tokens) Revoke(ctx context.Context, id string) error {
	key := store.Key{Kind: internalv1.KindAPIToken, Name: id}
	if err := t.st.Delete(ctx, key, store.DeleteOptions{Manager: store.ManagerSystem, Force: true}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	t.toks.Remove(key)
	t.log.InfoContext(ctx, "api token revoked", "token", id)
	return nil
}

// RevokeOwner deletes every token of ownerID and returns how many.
func (t *Tokens) RevokeOwner(ctx context.Context, ownerID string) (int, error) {
	n := 0
	for _, tok := range t.List(ownerID) {
		if err := t.Revoke(ctx, tok.Name); err != nil && !errors.Is(err, ErrNotFound) {
			return n, err
		}
		n++
	}
	return n, nil
}

// ServiceAccountID is the Principal.ID of a kudurru service account.
func ServiceAccountID(namespace, name string) string { return "sa:" + namespace + "/" + name }

func saKey(namespace, name string) store.Key {
	return store.Key{Kind: internalv1.KindServiceAccount, Namespace: namespace, Name: name}
}

func (t *Tokens) serviceAccount(namespace, name string) (*internalv1.ServiceAccount, bool) {
	return t.sas.Get(saKey(namespace, name))
}

// EnsureServiceAccount creates the service account when it is missing and
// returns it.
func (t *Tokens) EnsureServiceAccount(ctx context.Context, namespace, name, description string, builtinAdmin bool) (*internalv1.ServiceAccount, error) {
	var cur internalv1.ServiceAccount
	err := t.st.Get(ctx, saKey(namespace, name), &cur)
	if err == nil {
		t.sas.Upsert(&cur)
		return &cur, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	sa := &internalv1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: internalv1.ServiceAccountSpec{Description: description, BuiltinAdmin: builtinAdmin}}
	if err := sa.Validate(); err != nil {
		return nil, err
	}
	out, err := t.st.Apply(ctx, sa, store.ApplyOptions{Manager: store.ManagerSystem})
	if err != nil {
		return nil, err
	}
	created := out.(*internalv1.ServiceAccount)
	t.sas.Upsert(created)
	return created, nil
}

// Authenticate implements auth.Authenticator: the bearer kdr_ step.
func (t *Tokens) Authenticate(_ http.ResponseWriter, r *http.Request) (auth.Principal, *http.Request, error) {
	raw := auth.BearerToken(r)
	if !strings.HasPrefix(raw, Prefix) {
		return auth.Principal{}, nil, auth.ErrNoCredentials
	}
	p, err := t.Verify(r.Context(), raw, clientIP(r))
	if err != nil {
		return auth.Principal{}, nil, err
	}
	return p, r, nil
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Verify checks a wire token and returns the principal it acts as. Every
// failure is the same auth.Unauthenticated error (unknown id, wrong
// secret, malformed, expired, owner gone or disabled); the cause is only
// logged.
func (t *Tokens) Verify(_ context.Context, wire, ip string) (auth.Principal, error) {
	id, ok := Parse(wire)
	want := dummyHash
	tok, known := (*internalv1.APIToken)(nil), false
	if ok {
		if tok, known = t.Get(id); known {
			want = tok.Spec.SecretHash
		}
	}
	got := Hash(wire)
	match := subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
	if !ok || !known || !match {
		return auth.Principal{}, auth.Unauthenticated(errors.New("unknown token or wrong secret"))
	}
	now := t.opts.Now()
	if tok.Spec.ExpiresAt != nil && !now.Before(tok.Spec.ExpiresAt.Time) {
		return auth.Principal{}, auth.Unauthenticated(errors.New("token expired"))
	}
	p := auth.Principal{AuthMethod: auth.MethodToken, TokenID: id, Scopes: slices.Clone(tok.Spec.Scopes), CredentialIssuedAt: tok.CreationTimestamp.Time}
	owner := tok.Spec.Owner
	switch owner.Kind {
	case internalv1.OwnerUser:
		p.Kind, p.ID, p.Name, p.Provider = auth.KindUser, owner.ID, owner.Name, owner.Provider
		if p.Provider == "" && strings.HasPrefix(owner.ID, "local:") {
			// Tokens minted from a session that did not carry the provider
			// still belong to the local user store (LoadPrincipal re-reads
			// the builtin-admin flag, groups and the disabled state).
			p.Provider = "local"
		}
		if len(tok.Spec.Grants) > 0 {
			p.Grants = slices.Clone(tok.Spec.Grants)
			p.GrantsIssuedAt = tok.CreationTimestamp.Time
		}
	case internalv1.OwnerServiceAccount:
		sa, ok := t.serviceAccount(owner.Namespace, owner.Name)
		if !ok || sa.Spec.Disabled {
			return auth.Principal{}, auth.Unauthenticated(errors.New("service account missing or disabled"))
		}
		// A token older than the account belongs to a deleted account of
		// the same name (whole-second timestamps on both sides).
		if created := sa.CreationTimestamp.Time; !created.IsZero() && !tok.CreationTimestamp.IsZero() && tok.CreationTimestamp.Unix() < created.Unix() {
			return auth.Principal{}, auth.Unauthenticated(errors.New("token predates its service account"))
		}
		p.Kind, p.ID = auth.KindServiceAccount, ServiceAccountID(owner.Namespace, owner.Name)
		p.Name = "serviceaccount:" + owner.Namespace + ":" + owner.Name
		p.ServiceAccount = owner.Namespace + "/" + owner.Name
		p.Provider = "kudurru"
		p.BuiltinAdmin = sa.Spec.BuiltinAdmin
	default:
		return auth.Principal{}, auth.Unauthenticated(errors.New("token owner kind unknown"))
	}
	t.noteUse(id, ip, now)
	return p, nil
}

func (t *Tokens) noteUse(id, ip string, now time.Time) {
	t.usedMu.Lock()
	last, seen := t.used[id]
	if seen && now.Sub(last) < lastUsedInterval {
		t.usedMu.Unlock()
		return
	}
	if len(t.used) > 100_000 {
		clear(t.used)
	}
	t.used[id] = now
	t.usedMu.Unlock()
	select {
	case t.usedCh <- usage{id: id, ip: ip, at: now}:
	default: // the recorder is behind or not running; lastUsedAt is best effort
	}
}
