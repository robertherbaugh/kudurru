package security

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/auth"
	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const factorChallengeLifetime = 5 * time.Minute
const maxFactorChallenge = 8 << 10

// FactorSetup contains a short-lived enrollment challenge. It becomes safe to
// return only after BeginTOTP's enclosing command commits. Generic diagnostics
// redact it; Disclosure is the intentional authenticated response projection.
type FactorSetup struct {
	challenge, secret   string
	issuedAt, expiresAt time.Time
}

func (FactorSetup) String() string { return "private factor enrollment" }

// GoString redacts both the provisioning seed and sealed challenge.
func (s FactorSetup) GoString() string { return s.String() }

// MarshalJSON prevents accidental response or diagnostic serialization.
func (FactorSetup) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Disclosure returns the enrollment challenge and Base32 provisioning secret.
// These are credential material for the enrolling user; never log them.
func (s FactorSetup) Disclosure() (challenge, secret string) { return s.challenge, s.secret }

// ExpiresAt returns the original absolute enrollment challenge deadline.
func (s FactorSetup) ExpiresAt() time.Time { return s.expiresAt }

// ValidAt checks only immutable challenge bounds. It grants no authority to
// disclose an uncommitted setup or to enroll a factor.
func (s FactorSetup) ValidAt(now time.Time) error {
	return (factorValidity{observedAt: s.issuedAt, issuedAt: s.issuedAt, expiresAt: s.expiresAt}).validAt(now)
}

type factorChallenge struct {
	Version      int          `json:"version"`
	Account      totp.Account `json:"account"`
	RV           string       `json:"rv"`
	KeyIdentity  string       `json:"keyIdentity"`
	Dependency   string       `json:"dependency"`
	PriorVersion uint64       `json:"priorVersion,string"`
	PriorID      string       `json:"priorID"`
	Seed         string       `json:"seed"`
	IssuedAt     time.Time    `json:"issuedAt"`
	ExpiresAt    time.Time    `json:"expiresAt"`
}

// BeginTOTP verifies the current local password and any existing factor before
// generating a new seed. It never activates the candidate seed. An existing
// factor's proof is consumed in this command, so rotation confirmation needs a
// later valid code. Outer admission supplies current dependencies, rate limits
// and CSRF, and MUST propagate errors to the enclosing Store.Command.
func BeginTOTP(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, now time.Time) (FactorSetup, error) {
	u, err := factorAccount(ctx, tx, deps, proof, now)
	if err != nil {
		return FactorSetup{}, err
	}
	s, err := Load(ctx, tx)
	if err != nil {
		return FactorSetup{}, err
	}
	account := totp.Account{Name: u.Name, UID: string(u.UID)}
	previous, _ := s.factor(account)
	if previous.SeedVersion == math.MaxUint64 {
		return FactorSetup{}, servingauth.ErrBounds
	}
	seed, err := totp.Generate(account, previous.SeedVersion+1)
	if err != nil {
		return FactorSetup{}, err
	}
	sealed, err := deps.TOTPKeys.Seal(seed)
	if err != nil {
		return FactorSetup{}, err
	}
	sealer, err := factorSealer(deps)
	if err != nil {
		return FactorSetup{}, err
	}
	c := factorChallenge{Version: 1, Account: account, RV: u.ResourceVersion, KeyIdentity: deps.TOTPKeys.Identity(), Dependency: deps.Identity,
		PriorVersion: previous.SeedVersion, PriorID: previous.SeedID, Seed: sealed, IssuedAt: now.UTC(), ExpiresAt: now.Add(factorChallengeLifetime).UTC()}
	raw, err := json.Marshal(c)
	if err != nil || session.SealedLen(len(raw)) > maxFactorChallenge {
		return FactorSetup{}, servingauth.ErrBounds
	}
	return FactorSetup{challenge: sealer.Seal(raw), secret: seed.ProvisioningSecret(), issuedAt: c.IssuedAt, expiresAt: c.ExpiresAt}, nil
}

// ConfirmTOTP rechecks current password/old factor and proves possession of the
// candidate seed before atomically installing it, consuming its code and
// revoking existing sessions. The prior replay floor survives seed rotation.
// A failed callback rolls back both old and new proof consumption.
func ConfirmTOTP(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, challenge, code string, now time.Time) error {
	c, seed, err := openFactor(deps, challenge, now)
	if err != nil {
		return err
	}
	return confirmTOTP(ctx, tx, deps, proof, c, seed, code, now)
}

func confirmTOTP(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, c factorChallenge, seed *totp.Seed, code string, now time.Time) error {
	// Capture the durable floor before verification can stage old-factor use.
	before, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	prior, _ := before.factor(c.Account)
	if prior.SeedVersion != c.PriorVersion || prior.SeedID != c.PriorID {
		return ErrConflict
	}
	step, err := totp.Match(seed, code, now)
	if err != nil {
		return servingauth.ErrCredentials
	}
	account, version, id := seed.Identity()
	next, err := totp.Advance(totp.Record{FormatVersion: totp.FormatVersion, Account: account, SeedVersion: version, SeedID: id, Enabled: true,
		HasConsumed: prior.HasConsumed, LastConsumedStep: prior.LastConsumedStep}, seed, step)
	if err != nil {
		return servingauth.ErrCredentials
	}
	u, err := factorAccount(ctx, tx, deps, proof, now)
	if err != nil {
		return err
	}
	if c.Account != (totp.Account{Name: u.Name, UID: string(u.UID)}) || c.RV != u.ResourceVersion {
		return ErrConflict
	}
	s, err := Load(ctx, tx) // preserve staged old-factor consumption.
	if err != nil {
		return err
	}
	current, _ := s.factor(c.Account)
	if current.HasConsumed && current.LastConsumedStep > next.LastConsumedStep {
		next.LastConsumedStep = current.LastConsumedStep
	}
	if err := s.setFactor(next); err != nil {
		return err
	}
	u.Spec.TOTPSecret = c.Seed
	return s.commitFactor(ctx, u, now)
}

// DisableTOTP removes only the authenticated user's factor. It requires the
// current password and enabled factor; its replay authority is retained.
func DisableTOTP(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, now time.Time) error {
	u, err := factorAccount(ctx, tx, deps, proof, now)
	if err != nil {
		return err
	}
	return disableFactor(ctx, tx, u, now)
}

// RecoverTOTP is the separate administrator recovery command. Raw proof is
// authorized against current users/write rights, including token restrictions,
// before reading the exact target UID. It never enrolls a replacement factor.
func RecoverTOTP(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, target totp.Account, now time.Time) error {
	if totp.Key(target) == "" {
		return servingauth.ErrInvalid
	}
	if _, _, err := servingauth.ReauthorizeCurrent(ctx, tx, deps, proof, rbac.Attributes{Resource: "users", Verb: "write", Name: target.Name}, now); err != nil {
		return err
	}
	return recoverTOTP(ctx, tx, target, now)
}

func recoverTOTP(ctx context.Context, tx *sqlite.CommandTx, target totp.Account, now time.Time) error {
	var u internalv1.LocalUser
	if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: target.Name}, &u); err != nil {
		return err
	}
	if string(u.UID) != target.UID || u.DeletionTimestamp != nil {
		return ErrConflict
	}
	return disableFactor(ctx, tx, &u, now)
}

func factorAccount(ctx context.Context, tx *sqlite.CommandTx, deps servingauth.Dependencies, proof servingauth.Credential, now time.Time) (*internalv1.LocalUser, error) {
	if proof.Kind != servingauth.Password || deps.TOTPKeys == nil || now.IsZero() || now.Unix() < 0 || now.Add(factorChallengeLifetime).Year() > 9999 {
		return nil, servingauth.ErrUnsupported
	}
	d, err := servingauth.ResolveSessionCurrent(ctx, tx, deps, proof, now)
	if err != nil {
		return nil, err
	}
	if _, err := localLoginConfig(d); err != nil {
		return nil, err
	}
	p := d.Principal
	if p.Kind != auth.KindUser || p.Provider != "local" || p.AccountUID == "" || p.MustChangePassword {
		return nil, servingauth.ErrDenied
	}
	var u internalv1.LocalUser
	if err := tx.Get(ctx, store.Key{Kind: "LocalUser", Name: p.Name}, &u); err != nil {
		return nil, err
	}
	if string(u.UID) != p.AccountUID || u.ResourceVersion == "" {
		return nil, ErrConflict
	}
	return &u, nil
}

func factorSealer(deps servingauth.Dependencies) (*session.Sealer, error) {
	return session.NewSealer("totp-enrollment/v1", deps.CurrentKey, deps.PreviousKey)
}

func openFactor(deps servingauth.Dependencies, sealed string, now time.Time) (factorChallenge, *totp.Seed, error) {
	var c factorChallenge
	if len(sealed) == 0 || len(sealed) > maxFactorChallenge || deps.TOTPKeys == nil || now.IsZero() {
		return c, nil, servingauth.ErrCredentials
	}
	s, err := factorSealer(deps)
	if err != nil {
		return c, nil, servingauth.ErrCredentials
	}
	raw, _, err := s.Open(sealed)
	if err != nil || len(raw) > maxFactorChallenge {
		return c, nil, servingauth.ErrCredentials
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(new(any)) != io.EOF || c.Version != 1 || c.RV == "" || c.IssuedAt.IsZero() || c.IssuedAt.After(now) || !now.Before(c.ExpiresAt) || !c.ExpiresAt.Equal(c.IssuedAt.Add(factorChallengeLifetime)) || c.PriorVersion == math.MaxUint64 || c.KeyIdentity != deps.TOTPKeys.Identity() || c.Dependency != deps.Identity || c.Dependency != deps.ExpectedIdentity {
		return c, nil, servingauth.ErrCredentials
	}
	seed, err := deps.TOTPKeys.Open(c.Account, c.Seed)
	if err != nil {
		return c, nil, servingauth.ErrCredentials
	}
	_, version, _ := seed.Identity()
	if version != c.PriorVersion+1 {
		return c, nil, servingauth.ErrCredentials
	}
	return c, seed, nil
}

func (s *State) factor(account totp.Account) (totp.Record, bool) {
	for _, r := range s.value.TOTP {
		if r.Account == account {
			return r, true
		}
	}
	return totp.Record{}, false
}

func (s *State) setFactor(record totp.Record) error {
	if _, err := totp.EncodeRecord(record); err != nil {
		return err
	}
	for i, r := range s.value.TOTP {
		if r.Account == record.Account {
			if record.SeedVersion < r.SeedVersion || r.HasConsumed && (!record.HasConsumed || record.LastConsumedStep < r.LastConsumedStep) {
				return ErrConflict
			}
			s.value.TOTP[i], s.changed = record, true
			return nil
		}
	}
	if err := s.capacity(); err != nil {
		return err
	}
	s.value.TOTP, s.changed = append(s.value.TOTP, record), true
	return nil
}

func disableFactor(ctx context.Context, tx *sqlite.CommandTx, u *internalv1.LocalUser, now time.Time) error {
	s, err := Load(ctx, tx)
	if err != nil {
		return err
	}
	if u.Spec.TOTPSecret == "" {
		return nil // current authentication/admission still ran.
	}
	r, exists := s.factor(totp.Account{Name: u.Name, UID: string(u.UID)})
	if !exists || !r.Enabled {
		return servingauth.ErrManifest
	}
	r.Enabled = false
	if err := s.setFactor(r); err != nil {
		return err
	}
	u.Spec.TOTPSecret = ""
	return s.commitFactor(ctx, u, now)
}

func (s *State) commitFactor(ctx context.Context, u *internalv1.LocalUser, now time.Time) error {
	if _, err := s.tx.Apply(ctx, u, store.ApplyOptions{Manager: store.ManagedBy(u), ExpectedResourceVersion: u.ResourceVersion}); err != nil {
		return err
	}
	if err := s.InvalidateAccount(accountRef(u), now); err != nil {
		return err
	}
	return s.Commit(ctx)
}
