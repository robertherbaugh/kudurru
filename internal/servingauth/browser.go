package servingauth

import (
	"context"
	"encoding/base64"
	"math"
	"net/url"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/browseroidc"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// Browser attempts are explicit pending authority, never replay tombstones or
// authenticated sessions. Absence always denies consumption, including after GC.
const (
	MaxBrowserAttempts = 256
	BrowserAttemptTTL  = 10 * time.Minute
)

// BrowserAttempt binds authenticated preauth plaintext to one exact provider
// capability. Only its digest is durable; nonce, PKCE verifier, code and tokens
// are never persisted here. The enclosing security root must inventory the row.
type BrowserAttempt struct {
	FormatVersion    int         `json:"formatVersion"`
	ID               string      `json:"id"`
	Provider         ProviderRef `json:"provider"`
	SpecIdentity     string      `json:"specIdentity"`
	ResourceIdentity string      `json:"resourceIdentity"`
	ClientID         string      `json:"clientID"`
	RedirectURI      string      `json:"redirectURI"`
	IssuedAt         time.Time   `json:"issuedAt"`
	ExpiresAt        time.Time   `json:"expiresAt"`
	PayloadHash      string      `json:"payloadHash"`
}

type rootV3Entry struct {
	FormatVersion      int      `json:"formatVersion"`
	Epoch              uint64   `json:"epoch,string"`
	GrantFloors        []string `json:"grantFloors"`
	AccountFloors      []string `json:"accountFloors"`
	ProviderFloors     []string `json:"providerFloors"`
	Sessions           []string `json:"sessions"`
	TOTP               []string `json:"totp"`
	ExternalIdentities []string `json:"externalIdentities"`
	ExternalSessions   []string `json:"externalSessions"`
}

// UpgradeSecurityV3 accepts only a complete frozen v3 owner. The explicit empty
// pending inventory and epoch advance are staged by root in one startup command.
// Ordinary decoding never adopts missing inventory or calls this migration.
func UpgradeSecurityV3(rows []replication.Entry) (Security, error) {
	s, err := decodeSecurity(rows, 3)
	if err != nil {
		return Security{}, err
	}
	if s.Epoch == math.MaxUint64 {
		return Security{}, ErrBounds
	}
	s.Epoch++
	return s, nil
}

// BrowserAttemptKey contains only a random public identifier, never a secret.
func BrowserAttemptKey(id string) string { return "auth/v1/browser/" + id }

func validBrowserID(id string) bool {
	if len(id) != 43 {
		return false
	}
	b, err := base64.RawURLEncoding.Strict().DecodeString(id)
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == id
}

func validBrowserAttempt(a BrowserAttempt) bool {
	if a.FormatVersion != FormatVersion || !validBrowserID(a.ID) || !validProviderRef(a.Provider) ||
		!hashPattern.MatchString(a.SpecIdentity) || !hashPattern.MatchString(a.ResourceIdentity) || !hashPattern.MatchString(a.PayloadHash) ||
		!safeText(a.ClientID, 1024, false) || !safeText(a.RedirectURI, 2048, false) ||
		!validTime(a.IssuedAt) || !validTime(a.ExpiresAt) || !a.ExpiresAt.Equal(a.IssuedAt.Add(BrowserAttemptTTL)) {
		return false
	}
	u, err := url.Parse(a.RedirectURI)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}

// BrowserAttempt returns detached, currently eligible pending evidence from
// this exact View. Callers must still compare the authenticated payload hash and
// consume the complete row in the actual current issuance command.
func (v *View) BrowserAttempt(id string, now time.Time) (BrowserAttempt, error) {
	if v == nil || v.released.Load() || !validBrowserID(id) {
		return BrowserAttempt{}, ErrCredentials
	}
	for _, attempt := range v.cut.security.BrowserAttempts {
		if attempt.ID == id {
			if err := v.checkBrowserProvider(attempt, now); err != nil {
				return BrowserAttempt{}, err
			}
			return attempt, nil
		}
	}
	return BrowserAttempt{}, ErrCredentials
}

// SealBrowserPreauth requires the exact pending row in this complete postcommit
// View. It never exports sealing keys or treats pending evidence as a session.
func (v *View) SealBrowserPreauth(state browseroidc.State, now time.Time) (string, error) {
	if err := v.browserState(state, now); err != nil {
		return "", err
	}
	return v.browserCodec.Seal(state, now)
}

// OpenBrowserPreauth uses only this View's purpose-separated captured keys and
// pending inventory. Consuming the current row still requires IssueBrowser in
// the actual command; an old held View cannot grant a second enrollment.
func (v *View) OpenBrowserPreauth(raw string, now time.Time) (browseroidc.State, error) {
	if v == nil || v.released.Load() || v.browserCodec == nil {
		return browseroidc.State{}, ErrCredentials
	}
	state, err := v.browserCodec.Open(raw, now)
	if err != nil {
		return browseroidc.State{}, ErrCredentials
	}
	if err := v.browserState(state, now); err != nil {
		return browseroidc.State{}, err
	}
	return state, nil
}

func (v *View) browserState(state browseroidc.State, now time.Time) error {
	if v == nil || v.released.Load() || v.browserCodec == nil || state.ValidAt(now) != nil {
		return ErrCredentials
	}
	a, err := v.BrowserAttempt(state.ID(), now)
	if err != nil {
		return err
	}
	if a.Provider.ID != state.Provider() || !a.IssuedAt.Equal(state.IssuedAt()) || !a.ExpiresAt.Equal(state.ExpiresAt()) || a.PayloadHash != state.PayloadHash() {
		return ErrCredentials
	}
	return nil
}

// CheckBrowserProviderCurrent verifies the attempt's exact provider tuple,
// readiness, entitlement and floor against this transaction. It does not assert
// that a pending row exists and grants no user authority. Root uses it before
// creating or consuming a pending row and pins dependencies through commit.
func CheckBrowserProviderCurrent(ctx context.Context, tx *sqlite.CommandTx, deps Dependencies, attempt BrowserAttempt, now time.Time) error {
	if !validBrowserAttempt(attempt) {
		return ErrInvalid
	}
	v, err := prepareCurrent(ctx, tx, deps)
	if err != nil {
		return err
	}
	defer v.Release()
	return v.checkBrowserProvider(attempt, now)
}

func (v *View) checkBrowserProvider(a BrowserAttempt, now time.Time) error {
	if !validBrowserAttempt(a) || !validTime(now) || now.Before(a.IssuedAt) || !now.Before(a.ExpiresAt) {
		return ErrCredentials
	}
	p, err := v.externalProvider(a.Provider.ID)
	if err != nil {
		return err
	}
	if p.descriptor.Ref != a.Provider || p.descriptor.SpecIdentity != a.SpecIdentity || p.identity != a.ResourceIdentity ||
		p.descriptor.Spec.ClientID != a.ClientID || p.descriptor.Spec.RedirectURL != a.RedirectURI {
		return ErrDependency
	}
	if a.IssuedAt.Before(p.descriptor.CreatedAt) {
		return ErrCredentials
	}
	if floor := v.providers[providerKey(a.Provider.ID, a.Provider.UID)]; !floor.IsZero() && !a.IssuedAt.After(floor) {
		return ErrCredentials
	}
	return nil
}
