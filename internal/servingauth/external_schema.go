package servingauth

import (
	"bytes"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/auth/oidccore"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/util/validation"
)

// MaxExternalEvidenceBytes bounds private ciphertext before typed row allocation.
const MaxExternalEvidenceBytes = 56 << 10

var externalUIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

type rootV2Entry struct {
	FormatVersion  int      `json:"formatVersion"`
	Epoch          uint64   `json:"epoch,string"`
	GrantFloors    []string `json:"grantFloors"`
	AccountFloors  []string `json:"accountFloors"`
	ProviderFloors []string `json:"providerFloors"`
	Sessions       []string `json:"sessions"`
	TOTP           []string `json:"totp"`
}

// SecurityRootVersion is bounded explicit migration dispatch, never a decoder fallback.
func SecurityRootVersion(rows []replication.Entry) (int, error) {
	if len(rows) > MaxRows {
		return 0, ErrBounds
	}
	total := 0
	seen := map[string]bool{}
	var root []byte
	for _, row := range rows {
		total += len(row.Data)
		limit := MaxRowBytes
		if row.Key == RootKey {
			limit = MaxRootBytes
		}
		if len(row.Key) > 512 || len(row.Data) > limit || total > MaxSecurityBytes {
			return 0, ErrBounds
		}
		if !strings.HasPrefix(row.Key, ownerPrefix) {
			continue
		}
		if seen[row.Key] {
			return 0, ErrInvalid
		}
		seen[row.Key] = true
		if row.Key == RootKey {
			root = row.Data
		}
	}
	if root == nil {
		return 0, ErrManifest
	}
	if !boundedJSON(root, MaxRootBytes) {
		return 0, ErrInvalid
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(root, &fields) != nil {
		return 0, ErrInvalid
	}
	for key := range fields {
		if strings.EqualFold(key, "formatVersion") && key != "formatVersion" {
			return 0, ErrInvalid
		}
	}
	var version int
	if json.Unmarshal(fields["formatVersion"], &version) != nil || version < 1 {
		return 0, ErrVersion
	}
	return version, nil
}

// UpgradeSecurityV2 accepts only a complete frozen v2 owner and advances once.
func UpgradeSecurityV2(rows []replication.Entry) (Security, error) {
	s, err := decodeSecurity(rows, 2)
	if err != nil {
		return Security{}, err
	}
	if s.Epoch == math.MaxUint64 {
		return Security{}, ErrBounds
	}
	s.Epoch++
	return s, nil
}

func validProviderRef(p ProviderRef) bool {
	return len(p.ID) <= 63 && len(p.UID) <= 512 && len(p.Origin) <= 6 && len(validation.IsDNS1123Label(p.ID)) == 0 && p.ID != "local" && p.ID != "kudurru" && p.ID != "unix" && p.ID != "internal" && p.ID != "kubernetes" && safeText(p.UID, 512, false) && (p.Origin == ProviderConfig || p.Origin == ProviderObject)
}
func safeText(s string, limit int, empty bool) bool {
	if len(s) > limit || !empty && s == "" || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}
func validExternalTuple(p ProviderRef, issuer, subject string) bool {
	if !validProviderRef(p) || !safeText(issuer, oidccore.MaxIssuerBytes, false) || !safeText(subject, oidccore.MaxSubjectBytes, false) {
		return false
	}
	u, err := url.Parse(issuer)
	return err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.Fragment == ""
}

// ExternalIdentityName is a canonical tuple digest, not a display-name identity.
func ExternalIdentityName(p ProviderRef, issuer, subject string) (string, error) {
	if !validExternalTuple(p, issuer, subject) {
		return "", ErrInvalid
	}
	return hash(struct {
		Version         int
		Provider        ProviderRef
		Issuer, Subject string
	}{1, p, issuer, subject}), nil
}

// ExternalIdentityKey names the exact provider-incarnation identity row.
func ExternalIdentityKey(p ProviderRef, issuer, subject string) (string, error) {
	name, err := ExternalIdentityName(p, issuer, subject)
	if err != nil {
		return "", err
	}
	return "auth/v1/external-identity/" + name, nil
}

// ExternalAccount binds revocation to the independently generated identity UID.
func ExternalAccount(i ExternalIdentity) (AccountRef, error) {
	name, err := ExternalIdentityName(i.Provider, i.Issuer, i.Subject)
	if err != nil || !externalUIDPattern.MatchString(i.UID) {
		return AccountRef{}, ErrInvalid
	}
	return AccountRef{Kind: "ExternalIdentity", Name: name, UID: i.UID}, nil
}

// ExternalSessionKey names a credential without using its secret.
func ExternalSessionKey(id string) string { return "auth/v1/external-session/" + id }

func validExternalIdentity(i ExternalIdentity) bool {
	if i.FormatVersion != FormatVersion || !validExternalTuple(i.Provider, i.Issuer, i.Subject) || !externalUIDPattern.MatchString(i.UID) || !validTime(i.CreatedAt) || !safeText(i.Username, oidccore.MaxNameBytes, true) || !safeText(i.DisplayName, oidccore.MaxNameBytes, true) || !safeText(i.Email, oidccore.MaxNameBytes, true) || i.Groups == nil || len(i.Groups) > oidccore.MaxGroups {
		return false
	}
	if i.GroupsVerifiedAt == nil && len(i.Groups) != 0 || i.GroupsVerifiedAt != nil && (!validTime(*i.GroupsVerifiedAt) || i.GroupsVerifiedAt.Before(i.CreatedAt)) {
		return false
	}
	for j, g := range i.Groups {
		if !safeText(g, oidccore.MaxNameBytes, false) || slices.Contains(i.Groups[:j], g) {
			return false
		}
	}
	return true
}
func validExternalSession(s ExternalSessionRecord, epoch uint64) bool {
	if s.FormatVersion != FormatVersion || !sessionIDPattern.MatchString(s.ID) || !validExternalTuple(s.Provider, s.Issuer, s.Subject) || !validAccount(s.Account) || s.Account.Kind != "ExternalIdentity" || s.Epoch == 0 || s.Epoch > epoch || s.Grants == nil || len(s.Grants) > MaxGrants || !hashPattern.MatchString(s.CSRFHash) || !validTime(s.IssuedAt) || !validTime(s.ExpiresAt) || !validTime(s.IdleExpiresAt) || !s.ExpiresAt.After(s.IssuedAt) || !s.IdleExpiresAt.After(s.IssuedAt) || s.IdleExpiresAt.After(s.ExpiresAt) || s.Evidence == "" || len(s.Evidence) > MaxExternalEvidenceBytes {
		return false
	}
	name, _ := ExternalIdentityName(s.Provider, s.Issuer, s.Subject)
	if s.Account.Name != name {
		return false
	}
	for j, g := range s.Grants {
		if !validGrant(g) || slices.Contains(s.Grants[:j], g) {
			return false
		}
	}
	return true
}
func requiredFields(raw []byte, names ...string) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	for _, name := range names {
		b, ok := fields[name]
		if !ok {
			return false
		}
		if name == "disabled" || name == "revoked" {
			if !bytes.Equal(b, []byte("true")) && !bytes.Equal(b, []byte("false")) {
				return false
			}
		}
	}
	return true
}
func validateExternalRelations(s Security) error {
	ids := map[AccountRef]ExternalIdentity{}
	uids := map[string]bool{}
	sessions := map[string]bool{}
	for _, row := range s.Sessions {
		sessions[row.ID] = true
	}
	for _, i := range s.ExternalIdentities {
		a, err := ExternalAccount(i)
		if err != nil || uids[i.UID] {
			return ErrInvalid
		}
		uids[i.UID] = true
		ids[a] = i
	}
	for _, row := range s.ExternalSessions {
		i, ok := ids[row.Account]
		if !ok || sessions[row.ID] || i.Provider != row.Provider || i.Issuer != row.Issuer || i.Subject != row.Subject || row.IssuedAt.Before(i.CreatedAt) {
			return ErrInvalid
		}
		sessions[row.ID] = true
	}
	return nil
}
func validateExternalEntry(row replication.Entry) error {
	switch {
	case strings.HasPrefix(row.Key, "auth/v1/external-identity/"):
		var i ExternalIdentity
		if !strictOwner(row.Data, &i, MaxRowBytes) || !requiredFields(row.Data, "disabled", "groups", "groupsVerifiedAt") || !validExternalIdentity(i) {
			return ErrInvalid
		}
		key, _ := ExternalIdentityKey(i.Provider, i.Issuer, i.Subject)
		if key != row.Key {
			return ErrInvalid
		}
	case strings.HasPrefix(row.Key, "auth/v1/external-session/"):
		var s ExternalSessionRecord
		if !strictOwner(row.Data, &s, MaxRowBytes) || !canonicalEpoch(row.Data) || !requiredFields(row.Data, "revoked") || !validExternalSession(s, math.MaxUint64) || row.Key != ExternalSessionKey(s.ID) {
			return ErrInvalid
		}
	default:
		return ErrInvalid
	}
	return nil
}

func detachedExternal(i ExternalIdentity) ExternalIdentity {
	i.Groups = slices.Clone(i.Groups)
	if i.GroupsVerifiedAt != nil {
		t := *i.GroupsVerifiedAt
		i.GroupsVerifiedAt = &t
	}
	return i
}
