// Package users is the local account store of docs/architecture.md §8.7:
// LocalUser objects (api/internalkinds/v1alpha1) persisted through
// store.Store and mirrored in a watch-fed cache, Argon2id password hashes,
// the first-boot admin, password login with per-IP and per-user lockouts,
// and the sessionsValidAfter revocation lever the sealed session store
// consults on every request.
package users

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/objcache"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Provider is Principal.Provider for local accounts; PrincipalIDPrefix
// prefixes their Principal.ID ("local:alice").
const (
	Provider          = "local"
	PrincipalIDPrefix = "local:"
)

// Errors of Login and the password operations. The API maps
// ErrInvalidCredentials to 401, *LockedError to 429 with Retry-After,
// ErrDisabled to 403 and ErrNotFound to 404.
var (
	ErrInvalidCredentials = errors.New("users: invalid username or password")
	ErrDisabled           = errors.New("users: account disabled")
	ErrNotFound           = errors.New("users: no such user")
	ErrNoPassword         = errors.New("users: account has no password")
)

// LockedError is returned while a client IP or a username is locked out.
type LockedError struct {
	// By is "ip" or "user".
	By         string
	RetryAfter time.Duration
}

func (e *LockedError) Error() string {
	return fmt.Sprintf("users: too many failed logins (%s); retry in %s", e.By, e.RetryAfter.Round(time.Second))
}

// Options configures a Users store.
type Options struct {
	// Store persists LocalUser objects. Required.
	Store store.Store
	// Hasher hashes passwords; default NewHasher(0).
	Hasher *Hasher
	// IPLimit and UserLimit are the login lockout policies; defaults
	// DefaultIPLimit and DefaultUserLimit.
	IPLimit, UserLimit *LimitPolicy
	// Logger receives login and bootstrap lines; default discards.
	Logger *slog.Logger
	// Now is the clock; default time.Now.
	Now func() time.Time
}

// Users is the local account store. It is safe for concurrent use; reads
// come from the cache, so call Prime (and Run) before serving.
type Users struct {
	st     store.Store
	cache  *objcache.Cache[*internalv1.LocalUser]
	hasher *Hasher
	ipLim  *Limiter
	usrLim *Limiter
	log    *slog.Logger
	now    func() time.Time

	dummyOnce sync.Once
	dummy     string
}

// New returns a store; it does not touch the store until Prime.
func New(opts Options) (*Users, error) {
	if opts.Store == nil {
		return nil, errors.New("users: Options.Store is required")
	}
	if opts.Hasher == nil {
		opts.Hasher = NewHasher(0)
	}
	if opts.IPLimit == nil {
		opts.IPLimit = &DefaultIPLimit
	}
	if opts.UserLimit == nil {
		opts.UserLimit = &DefaultUserLimit
	}
	if opts.Logger == nil {
		opts.Logger = slog.New(slog.DiscardHandler)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	return &Users{
		st:     opts.Store,
		cache:  objcache.New[*internalv1.LocalUser](opts.Store, internalv1.KindLocalUser, objcache.Options{Logger: opts.Logger}),
		hasher: opts.Hasher,
		ipLim:  NewLimiter(*opts.IPLimit, opts.Now),
		usrLim: NewLimiter(*opts.UserLimit, opts.Now),
		log:    opts.Logger,
		now:    opts.Now,
	}, nil
}

// Prime loads every LocalUser into the cache.
func (u *Users) Prime(ctx context.Context) error { return u.cache.Prime(ctx) }

// Run keeps the cache current until ctx is done.
func (u *Users) Run(ctx context.Context) error { return u.cache.Run(ctx) }

// Hasher returns the password hasher.
func (u *Users) Hasher() *Hasher { return u.hasher }

// NormalizeUsername lower-cases and trims a login name; LocalUser names
// are DNS subdomains, so "Admin" and "admin" are the same account.
func NormalizeUsername(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// Get returns the cached user. The object is shared: do not modify it.
func (u *Users) Get(name string) (*internalv1.LocalUser, bool) {
	return u.cache.Get(store.Key{Kind: internalv1.KindLocalUser, Name: NormalizeUsername(name)})
}

// List returns every user sorted by name.
func (u *Users) List() []*internalv1.LocalUser {
	out := u.cache.List()
	slices.SortFunc(out, func(a, b *internalv1.LocalUser) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// HasDefaultCredentials reports whether a builtin admin still has to
// change its generated first-boot password (GET /auth/status).
func (u *Users) HasDefaultCredentials() bool {
	for _, usr := range u.cache.List() {
		if usr.Spec.BuiltinAdmin && usr.Spec.MustChangePassword && !usr.Spec.Disabled {
			return true
		}
	}
	return false
}

// PrincipalID returns the Principal.ID of a local user.
func PrincipalID(name string) string { return PrincipalIDPrefix + name }

// Principal returns the principal of a local user. Grants are left empty:
// local users are authorized from live bindings on every request.
func Principal(usr *internalv1.LocalUser) auth.Principal {
	return auth.Principal{
		Kind: auth.KindUser, ID: PrincipalID(usr.Name), Name: usr.Name, Provider: Provider,
		Groups: slices.Clone(usr.Spec.Groups), BuiltinAdmin: usr.Spec.BuiltinAdmin,
		MustChangePassword: usr.Spec.MustChangePassword,
	}
}

// ValidAfter is the session.ValidAfterFunc for local users: sessions of a
// local user issued at or before its status.sessionsValidAfter are
// revoked, and every session of a disabled or deleted user is. Other
// principals (OIDC users carry no LocalUser) get the zero time.
func (u *Users) ValidAfter(principalID string) time.Time {
	name, ok := strings.CutPrefix(principalID, PrincipalIDPrefix)
	if !ok {
		return time.Time{}
	}
	usr, ok := u.Get(name)
	if !ok || usr.Spec.Disabled {
		return farFuture
	}
	if t := usr.Status.SessionsValidAfter; t != nil {
		return t.Time
	}
	return time.Time{}
}

var farFuture = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// LoadPrincipal is the LoadPrincipal step for local users (§8.8): a
// principal of a deleted or disabled account is rejected, and the live
// groups, builtin-admin and mustChangePassword flags replace whatever the
// session or token carried. Other principals pass unchanged.
func (u *Users) LoadPrincipal(_ context.Context, p auth.Principal) (auth.Principal, error) {
	if p.Kind != auth.KindUser || p.Provider != Provider {
		return p, nil
	}
	usr, ok := u.Get(p.Name)
	if !ok || PrincipalID(usr.Name) != p.ID {
		return auth.Principal{}, auth.Unauthenticated(ErrNotFound)
	}
	if usr.Spec.Disabled {
		return auth.Principal{}, auth.Unauthenticated(ErrDisabled)
	}
	live := Principal(usr)
	live.AuthMethod, live.TokenID, live.Scopes = p.AuthMethod, p.TokenID, p.Scopes
	return live, nil
}

// SanitizeForLog renders an attacker-controlled username for a log line:
// at most 64 runes, control and non-printable runes replaced by '?'.
func SanitizeForLog(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range s {
		if n == 64 {
			b.WriteString("…")
			break
		}
		if !unicode.IsPrint(r) {
			r = '?'
		}
		b.WriteRune(r)
		n++
	}
	return b.String()
}

// dummyHash is verified for unknown users so a login for a missing
// account costs the same Argon2id work as one for an existing account.
func (u *Users) dummyHash(ctx context.Context) string {
	u.dummyOnce.Do(func() {
		h, err := u.hasher.Hash(context.WithoutCancel(ctx), GeneratePassword())
		if err == nil {
			u.dummy = h
		}
	})
	return u.dummy
}

// Login verifies a password login from clientIP (§8.7). The lockouts are
// checked first (429 without hashing), then the password is verified with
// the same Argon2id cost for known and unknown users; a failure counts
// against both the IP and the username. A correct password on a disabled
// account returns ErrDisabled. Success resets the username's counter (not
// the IP's: a valid account must not launder an IP's guesses) and records
// lastLoginAt in the background.
func (u *Users) Login(ctx context.Context, username, password, clientIP string) (*internalv1.LocalUser, error) {
	name := NormalizeUsername(username)
	if d := u.ipLim.Locked(clientIP); d > 0 {
		return nil, &LockedError{By: "ip", RetryAfter: d}
	}
	if d := u.usrLim.Locked(name); d > 0 {
		return nil, &LockedError{By: "user", RetryAfter: d}
	}
	usr, known := u.Get(name)
	hash := ""
	if known {
		hash = usr.Spec.PasswordHash
	}
	if hash == "" {
		hash = u.dummyHash(ctx)
	}
	ok, err := u.hasher.Verify(ctx, hash, password)
	if err != nil && !errors.Is(err, ErrBadHash) {
		return nil, err
	}
	if !ok || !known || usr.Spec.PasswordHash == "" {
		u.ipLim.Fail(clientIP)
		u.usrLim.Fail(name)
		u.log.WarnContext(ctx, "local login failed", "username", SanitizeForLog(username), "client", clientIP, "known", known)
		return nil, ErrInvalidCredentials
	}
	if usr.Spec.Disabled {
		u.log.WarnContext(ctx, "local login to a disabled account", "username", usr.Name, "client", clientIP)
		return nil, ErrDisabled
	}
	u.usrLim.Reset(name)
	u.log.InfoContext(ctx, "local login", "username", usr.Name, "client", clientIP)
	go u.recordLogin(context.WithoutCancel(ctx), usr.Name) //nolint:gosec // G118: detached on purpose; bounded by the timeout inside
	return usr, nil
}

func (u *Users) recordLogin(ctx context.Context, name string) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := metav1.NewTime(u.now().UTC().Truncate(time.Second))
	if err := u.updateStatus(ctx, name, func(s *internalv1.LocalUserStatus) { s.LastLoginAt = &now }); err != nil {
		u.log.DebugContext(ctx, "recording lastLoginAt failed", "username", name, "err", err)
	}
}

// Locked reports the remaining lockout for a client IP or username (for
// tests and diagnostics).
func (u *Users) Locked(clientIP, username string) time.Duration {
	return max(u.ipLim.Locked(clientIP), u.usrLim.Locked(NormalizeUsername(username)))
}

// CreateOptions describes a new local user.
type CreateOptions struct {
	Name, DisplayName  string
	Password           string
	Groups             []string
	MustChangePassword bool
	BuiltinAdmin       bool
	// Manager is the store manager of the first write; default system.
	Manager string
}

// Create adds a user; it fails with store.ErrManagedBy (from another
// manager) or ErrConflict-like errors when the user exists.
func (u *Users) Create(ctx context.Context, o CreateOptions) (*internalv1.LocalUser, error) {
	name := NormalizeUsername(o.Name)
	if _, exists := u.Get(name); exists {
		return nil, fmt.Errorf("users: %q already exists: %w", name, store.ErrConflict)
	}
	usr := &internalv1.LocalUser{ObjectMeta: metav1.ObjectMeta{Name: name}, Spec: internalv1.LocalUserSpec{
		DisplayName: o.DisplayName, Groups: o.Groups, MustChangePassword: o.MustChangePassword, BuiltinAdmin: o.BuiltinAdmin,
	}}
	if o.Password != "" {
		if err := CheckNewPassword(o.Password); err != nil {
			return nil, err
		}
		h, err := u.hasher.Hash(ctx, o.Password)
		if err != nil {
			return nil, err
		}
		usr.Spec.PasswordHash = h
	}
	if err := usr.Validate(); err != nil {
		return nil, err
	}
	mgr := o.Manager
	if mgr == "" {
		mgr = store.ManagerSystem
	}
	out, err := u.st.Apply(ctx, usr, store.ApplyOptions{Manager: mgr})
	if err != nil {
		return nil, err
	}
	created := out.(*internalv1.LocalUser)
	u.cache.Upsert(created)
	if o.Password != "" {
		now := metav1.NewTime(u.now().UTC().Truncate(time.Second))
		_ = u.updateStatus(ctx, name, func(s *internalv1.LocalUserStatus) { s.PasswordChangedAt = &now })
	}
	return created, nil
}

// SetPassword replaces the password (admin reset, `kudurru admin
// reset-password`, or ChangePassword after verification), sets
// mustChangePassword and revokes every existing session of the user.
func (u *Users) SetPassword(ctx context.Context, name, password string, mustChange bool) error {
	if err := CheckNewPassword(password); err != nil {
		return err
	}
	h, err := u.hasher.Hash(ctx, password)
	if err != nil {
		return err
	}
	if _, err := u.update(ctx, name, func(usr *internalv1.LocalUser) error {
		usr.Spec.PasswordHash = h
		usr.Spec.MustChangePassword = mustChange
		return nil
	}); err != nil {
		return err
	}
	now := metav1.NewTime(u.now().UTC().Truncate(time.Second))
	return u.updateStatus(ctx, name, func(s *internalv1.LocalUserStatus) {
		s.PasswordChangedAt = &now
		s.SessionsValidAfter = &now
	})
}

// ChangePassword verifies current and sets next (POST /auth/password). It
// clears mustChangePassword; the caller issues a fresh session, because
// every session up to now is revoked.
func (u *Users) ChangePassword(ctx context.Context, name, current, next string) error {
	usr, ok := u.Get(name)
	if !ok {
		return ErrNotFound
	}
	if usr.Spec.PasswordHash == "" {
		return ErrNoPassword
	}
	ok, err := u.hasher.Verify(ctx, usr.Spec.PasswordHash, current)
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidCredentials
	}
	if current == next {
		return errors.New("users: the new password must differ from the current one")
	}
	return u.SetPassword(ctx, usr.Name, next, false)
}

// RevokeSessions bumps sessionsValidAfter to now ("log out everywhere").
func (u *Users) RevokeSessions(ctx context.Context, name string) error {
	if _, ok := u.Get(name); !ok {
		return ErrNotFound
	}
	now := metav1.NewTime(u.now().UTC().Truncate(time.Second))
	return u.updateStatus(ctx, name, func(s *internalv1.LocalUserStatus) { s.SessionsValidAfter = &now })
}

// SetDisabled disables or enables an account; disabling revokes its
// sessions (ValidAfter treats disabled accounts as revoked) and its
// personal tokens stop working (the token step checks the owner).
func (u *Users) SetDisabled(ctx context.Context, name string, disabled bool) error {
	_, err := u.update(ctx, name, func(usr *internalv1.LocalUser) error {
		usr.Spec.Disabled = disabled
		return nil
	})
	return err
}

// update is a read-modify-write of the spec with optimistic concurrency
// (retried on conflict). Writes use Manager system with Force: the
// first-boot admin is created under a per-node bootstrap manager.
func (u *Users) update(ctx context.Context, name string, mutate func(*internalv1.LocalUser) error) (*internalv1.LocalUser, error) {
	name = NormalizeUsername(name)
	for attempt := 0; ; attempt++ {
		var cur internalv1.LocalUser
		if err := u.st.Get(ctx, store.Key{Kind: internalv1.KindLocalUser, Name: name}, &cur); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, ErrNotFound
			}
			return nil, err
		}
		next := cur.DeepCopy()
		if err := mutate(next); err != nil {
			return nil, err
		}
		out, err := u.st.Apply(ctx, next, store.ApplyOptions{Manager: store.ManagerSystem, Force: true, ExpectedResourceVersion: cur.ResourceVersion})
		if errors.Is(err, store.ErrConflict) && attempt < 5 {
			continue
		}
		if err != nil {
			return nil, err
		}
		usr := out.(*internalv1.LocalUser)
		u.cache.Upsert(usr)
		return usr, nil
	}
}

// updateStatus merges a status change into the current object.
func (u *Users) updateStatus(ctx context.Context, name string, mutate func(*internalv1.LocalUserStatus)) error {
	var cur internalv1.LocalUser
	if err := u.st.Get(ctx, store.Key{Kind: internalv1.KindLocalUser, Name: NormalizeUsername(name)}, &cur); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return ErrNotFound
		}
		return err
	}
	mutate(&cur.Status)
	cur.ResourceVersion = "" // status merges are last-writer-wins
	if err := u.st.UpdateStatus(ctx, &cur); err != nil {
		return err
	}
	var back internalv1.LocalUser
	if err := u.st.Get(ctx, store.KeyOf(&cur), &back); err == nil {
		u.cache.Upsert(&back)
	}
	return nil
}
