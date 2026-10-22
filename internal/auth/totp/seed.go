package totp

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Account binds a factor to one LocalUser incarnation.
type Account struct {
	Name string `json:"name"`
	UID  string `json:"uid"`
}

// Seed owns a private fixed-profile factor; only explicit provisioning discloses it.
type Seed struct {
	account Account
	version uint64
	id      string
	key     [20]byte
}

// String redacts seed material.
func (*Seed) String() string { return "private TOTP seed" }

// GoString redacts seed material.
func (s *Seed) GoString() string { return s.String() }

// MarshalJSON redacts seed material.
func (*Seed) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Identity returns detached public binding metadata, never a seed.
func (s *Seed) Identity() (Account, uint64, string) {
	if s == nil {
		return Account{}, 0, ""
	}
	return s.account, s.version, s.id
}

// ProvisioningSecret intentionally discloses enrollment credential material.
// Root may return it once through an authorized enrollment response; never log it.
func (s *Seed) ProvisioningSecret() string {
	if s == nil {
		return ""
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(s.key[:])
}

// Generate creates a new independent factor, without activating any account.
func Generate(account Account, version uint64) (*Seed, error) {
	if !validAccount(account) || version == 0 {
		return nil, ErrInvalid
	}
	s := &Seed{account: account, version: version}
	_, _ = rand.Read(s.key[:])
	var id [16]byte
	_, _ = rand.Read(id[:])
	s.id = hex.EncodeToString(id[:])
	return s, nil
}

// Keyring is immutable pure AEAD with a purpose distinct from session cookies.
type Keyring struct {
	sealer   *session.Sealer
	identity string
}

// String redacts private keys.
func (*Keyring) String() string { return "private TOTP keyring" }

// GoString redacts private keys.
func (k *Keyring) GoString() string { return k.String() }

// MarshalJSON redacts private keys.
func (*Keyring) MarshalJSON() ([]byte, error) { return []byte(`{"private":true}`), nil }

// Identity pins exact current/previous key resource identities.
func (k *Keyring) Identity() string {
	if k == nil {
		return ""
	}
	return k.identity
}

// NewKeyring derives and copies both keys; no caller slices are retained.
func NewKeyring(current, previous []byte) (*Keyring, error) {
	s, err := session.NewSealer(Purpose, current, previous)
	if err != nil {
		return nil, ErrInvalid
	}
	a, b := sha256.Sum256(current), sha256.Sum256(previous)
	raw := append(a[:], b[:]...)
	digest := sha256.Sum256(raw)
	return &Keyring{sealer: s, identity: hex.EncodeToString(digest[:])}, nil
}

type seedPayload struct {
	FormatVersion int     `json:"formatVersion"`
	Account       Account `json:"account"`
	SeedVersion   uint64  `json:"seedVersion,string"`
	SeedID        string  `json:"seedID"`
	Algorithm     string  `json:"algorithm"`
	Digits        int     `json:"digits"`
	Period        int     `json:"period"`
	Secret        string  `json:"secret"`
}

// Seal preserves factor identity when resealing under a new encryption key.
func (k *Keyring) Seal(s *Seed) (string, error) {
	if k == nil || k.sealer == nil || s == nil || !validAccount(s.account) || s.version == 0 || !validID(s.id) {
		return "", ErrInvalid
	}
	payload := seedPayload{FormatVersion: FormatVersion, Account: s.account, SeedVersion: s.version, SeedID: s.id, Algorithm: "SHA1", Digits: Digits, Period: Period, Secret: s.ProvisioningSecret()}
	raw, err := json.Marshal(payload) //nolint:gosec // G117: plaintext is used only by purpose-separated AEAD, never logs or response serialization.
	if err != nil || len(raw) > MaxSeedBytes || session.SealedLen(len(raw)) > MaxSealedBytes {
		return "", ErrBounds
	}
	return k.sealer.Seal(raw), nil
}

// Open requires an exact account incarnation and strict fixed-profile payload.
func (k *Keyring) Open(account Account, sealed string) (*Seed, error) {
	if k == nil || k.sealer == nil || !validAccount(account) || len(sealed) > MaxSealedBytes {
		return nil, ErrInvalid
	}
	raw, _, err := k.sealer.Open(sealed)
	if err != nil {
		return nil, ErrProof
	}
	var p seedPayload
	if err = decode(raw, &p, MaxSeedBytes); err != nil {
		return nil, err
	}
	if p.FormatVersion != FormatVersion {
		return nil, ErrVersion
	}
	if p.Account != account || !validAccount(p.Account) || p.SeedVersion == 0 || !validID(p.SeedID) || p.Algorithm != "SHA1" || p.Digits != Digits || p.Period != Period || len(p.Secret) != 32 {
		return nil, ErrInvalid
	}
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(p.Secret)
	if err != nil || len(key) != 20 || base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(key) != p.Secret {
		return nil, ErrInvalid
	}
	s := &Seed{account: p.Account, version: p.SeedVersion, id: p.SeedID}
	copy(s.key[:], key)
	return s, nil
}
func validAccount(a Account) bool {
	return len(a.Name) <= 253 && len(validation.IsDNS1123Subdomain(a.Name)) == 0 && a.UID != "" && len(a.UID) <= 512 && utf8.ValidString(a.UID) && !strings.ContainsRune(a.UID, 0)
}
func validID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := range id {
		if (id[i] < '0' || id[i] > '9') && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return true
}
func canonicalUint(raw json.RawMessage) bool {
	var wire string
	if json.Unmarshal(raw, &wire) != nil {
		return false
	}
	n, err := strconv.ParseUint(wire, 10, 64)
	return err == nil && strconv.FormatUint(n, 10) == wire
}
