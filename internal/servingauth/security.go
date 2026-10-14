package servingauth

import (
	"encoding/json"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/totp"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"k8s.io/apimachinery/pkg/util/validation"
)

var sessionIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
var hashPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// EmptySecurity is an explicit startup migration input. Root must commit its
// encoded root row transactionally before publisher activation; no read seeds it.
func EmptySecurity(epoch uint64) Security {
	return Security{Epoch: epoch, GrantFloors: []GrantFloor{}, AccountFloors: []AccountFloor{}, ProviderFloors: []ProviderFloor{}, Sessions: []SessionRecord{}, TOTP: []totp.Record{}, ExternalIdentities: []ExternalIdentity{}, ExternalSessions: []ExternalSessionRecord{}}
}

// EncodeSecurity returns deterministic owner rows including one mandatory root.
// All counts are checked before allocations; caller-supplied input is detached.
func EncodeSecurity(s Security) ([]replication.Entry, error) {
	if s.Epoch == 0 || s.GrantFloors == nil || s.AccountFloors == nil || s.ProviderFloors == nil || s.Sessions == nil || s.TOTP == nil || s.ExternalIdentities == nil || s.ExternalSessions == nil {
		return nil, ErrManifest
	}
	count := len(s.GrantFloors) + len(s.AccountFloors) + len(s.ProviderFloors) + len(s.Sessions) + len(s.TOTP) + len(s.ExternalIdentities) + len(s.ExternalSessions)
	if count >= MaxRows {
		return nil, ErrBounds
	}
	entries := make([]replication.Entry, 0, count+1)
	total := 0
	root := RootEntry{FormatVersion: RootFormatVersion, Epoch: s.Epoch, GrantFloors: []string{}, AccountFloors: []string{}, ProviderFloors: []string{}, Sessions: []string{}, TOTP: []string{}, ExternalIdentities: []string{}, ExternalSessions: []string{}}
	add := func(key string, value any, keys *[]string) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return ErrInvalid
		}
		if len(raw) > MaxRowBytes {
			return ErrBounds
		}
		total += len(raw)
		if total > MaxSecurityBytes {
			return ErrBounds
		}
		entries = append(entries, replication.Entry{Key: key, Data: raw})
		*keys = append(*keys, key)
		return nil
	}
	for _, row := range s.GrantFloors {
		if !validGrantFloor(row) {
			return nil, ErrInvalid
		}
		row.ValidAfter = row.ValidAfter.UTC()
		if err := add(grantKey(row.Grant), row, &root.GrantFloors); err != nil {
			return nil, err
		}
	}
	for _, row := range s.AccountFloors {
		if !validAccountFloor(row) {
			return nil, ErrInvalid
		}
		row.ValidAfter = row.ValidAfter.UTC()
		if err := add(accountKey(row.Account), row, &root.AccountFloors); err != nil {
			return nil, err
		}
	}
	for _, row := range s.ProviderFloors {
		if !validProviderFloor(row) {
			return nil, ErrInvalid
		}
		row.ValidAfter = row.ValidAfter.UTC()
		if err := add(providerKey(row.Provider, row.UID), row, &root.ProviderFloors); err != nil {
			return nil, err
		}
	}
	for _, row := range s.Sessions {
		if !validSession(row, s.Epoch) {
			return nil, ErrInvalid
		}
		row.IssuedAt = row.IssuedAt.UTC()
		row.ExpiresAt = row.ExpiresAt.UTC()
		row.IdleExpiresAt = row.IdleExpiresAt.UTC()
		row.Grants = slices.Clone(row.Grants)
		slices.Sort(row.Grants)
		if err := add(SessionKey(row.ID), row, &root.Sessions); err != nil {
			return nil, err
		}
	}
	for _, row := range s.TOTP {
		entry, err := totp.EncodeRecord(row)
		if err != nil {
			return nil, err
		}
		if err = add(entry.Key, row, &root.TOTP); err != nil {
			return nil, err
		}
	}
	for _, row := range s.ExternalIdentities {
		if !validExternalIdentity(row) {
			return nil, ErrInvalid
		}
		row.CreatedAt = row.CreatedAt.UTC()
		row.Groups = slices.Clone(row.Groups)
		slices.Sort(row.Groups)
		if row.GroupsVerifiedAt != nil {
			t := row.GroupsVerifiedAt.UTC()
			row.GroupsVerifiedAt = &t
		}
		key, _ := ExternalIdentityKey(row.Provider, row.Issuer, row.Subject)
		if err := add(key, row, &root.ExternalIdentities); err != nil {
			return nil, err
		}
	}
	for _, row := range s.ExternalSessions {
		if !validExternalSession(row, s.Epoch) {
			return nil, ErrInvalid
		}
		row.Grants = slices.Clone(row.Grants)
		slices.Sort(row.Grants)
		row.IssuedAt = row.IssuedAt.UTC()
		row.ExpiresAt = row.ExpiresAt.UTC()
		row.IdleExpiresAt = row.IdleExpiresAt.UTC()
		if err := add(ExternalSessionKey(row.ID), row, &root.ExternalSessions); err != nil {
			return nil, err
		}
	}
	slices.Sort(root.ExternalIdentities)
	slices.Sort(root.ExternalSessions)

	slices.Sort(root.GrantFloors)
	slices.Sort(root.AccountFloors)
	slices.Sort(root.ProviderFloors)
	slices.Sort(root.Sessions)
	slices.Sort(root.TOTP)
	raw, _ := json.Marshal(root)
	if len(raw) > MaxRootBytes {
		return nil, ErrBounds
	}
	entries = append(entries, replication.Entry{Key: RootKey, Data: raw})
	slices.SortFunc(entries, func(a, b replication.Entry) int { return strings.Compare(a.Key, b.Key) })
	// A single decoder enforces duplicates, manifest agreement and aggregate caps.
	if _, err := DecodeSecurity(entries); err != nil {
		return nil, err
	}
	return entries, nil
}

// DecodeSecurity strictly decodes the auth-owned subset. Other owners' rows are
// left to their validators; absent/null root sets never become empty state.
func DecodeSecurity(rows []replication.Entry) (Security, error) {
	return decodeSecurity(rows, RootFormatVersion)
}

// UpgradeSecurityV1 strictly migrates a COMPLETE legacy owner once. Root checks
// current LocalUsers for legacy active TOTP and stages the result explicitly.
// Normal decoders never fall back to this migration or seed a missing manifest.
func UpgradeSecurityV1(rows []replication.Entry) (Security, error) {
	s, err := decodeSecurity(rows, 1)
	if err != nil {
		return Security{}, err
	}
	if s.Epoch == math.MaxUint64 {
		return Security{}, ErrBounds
	}
	s.Epoch++
	return s, nil
}

type legacyRootEntry struct {
	FormatVersion  int      `json:"formatVersion"`
	Epoch          uint64   `json:"epoch,string"`
	GrantFloors    []string `json:"grantFloors"`
	AccountFloors  []string `json:"accountFloors"`
	ProviderFloors []string `json:"providerFloors"`
	Sessions       []string `json:"sessions"`
}

func decodeSecurity(rows []replication.Entry, version int) (Security, error) {
	if len(rows) > MaxRows {
		return Security{}, ErrBounds
	}
	total := 0
	for _, row := range rows {
		total += len(row.Data)
		if total > MaxSecurityBytes {
			return Security{}, ErrBounds
		}
		capBytes := MaxRowBytes
		if row.Key == RootKey {
			capBytes = MaxRootBytes
		}
		if len(row.Key) > 512 || len(row.Data) > capBytes {
			return Security{}, ErrBounds
		}
	}
	own := map[string]json.RawMessage{}
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, ownerPrefix) {
			continue
		}
		if !strings.HasPrefix(row.Key, "auth/v1/") {
			return Security{}, ErrVersion
		}
		if _, ok := own[row.Key]; ok {
			return Security{}, ErrInvalid
		}
		own[row.Key] = row.Data
	}
	var root RootEntry
	raw, ok := own[RootKey]
	if !ok {
		return Security{}, ErrManifest
	}
	expectedVersion := version
	if version == 1 {
		var old legacyRootEntry
		if !strictOwner(raw, &old, MaxRootBytes) {
			return Security{}, ErrInvalid
		}
		root = RootEntry{FormatVersion: old.FormatVersion, Epoch: old.Epoch, GrantFloors: old.GrantFloors, AccountFloors: old.AccountFloors, ProviderFloors: old.ProviderFloors, Sessions: old.Sessions, TOTP: []string{}, ExternalIdentities: []string{}, ExternalSessions: []string{}}
		expectedVersion = 1
	} else if version == 2 {
		var old rootV2Entry
		if !strictOwner(raw, &old, MaxRootBytes) {
			return Security{}, ErrInvalid
		}
		root = RootEntry{FormatVersion: old.FormatVersion, Epoch: old.Epoch, GrantFloors: old.GrantFloors, AccountFloors: old.AccountFloors, ProviderFloors: old.ProviderFloors, Sessions: old.Sessions, TOTP: old.TOTP, ExternalIdentities: []string{}, ExternalSessions: []string{}}
	} else if !strictOwner(raw, &root, MaxRootBytes) {
		return Security{}, ErrInvalid
	}
	if root.FormatVersion != expectedVersion {
		return Security{}, ErrVersion
	}
	if !canonicalEpoch(raw) || root.Epoch == 0 || root.GrantFloors == nil || root.AccountFloors == nil || root.ProviderFloors == nil || root.Sessions == nil || root.TOTP == nil || root.ExternalIdentities == nil || root.ExternalSessions == nil {
		return Security{}, ErrManifest
	}
	count := len(root.GrantFloors) + len(root.AccountFloors) + len(root.ProviderFloors) + len(root.Sessions) + len(root.TOTP) + len(root.ExternalIdentities) + len(root.ExternalSessions)
	if count >= MaxRows {
		return Security{}, ErrBounds
	}
	if count+1 != len(own) {
		return Security{}, ErrManifest
	}
	s := EmptySecurity(root.Epoch)
	seen := map[string]bool{RootKey: true}
	read := func(key string, dst any) error {
		if seen[key] {
			return ErrInvalid
		}
		seen[key] = true
		raw, ok := own[key]
		if !ok {
			return ErrManifest
		}
		if !strictOwner(raw, dst, MaxRowBytes) {
			return ErrInvalid
		}
		return nil
	}
	for _, key := range root.GrantFloors {
		var row GrantFloor
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		if row.FormatVersion != FormatVersion {
			return Security{}, ErrVersion
		}
		if !validGrantFloor(row) || key != grantKey(row.Grant) {
			return Security{}, ErrInvalid
		}
		s.GrantFloors = append(s.GrantFloors, row)
	}
	for _, key := range root.AccountFloors {
		var row AccountFloor
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		if row.FormatVersion != FormatVersion {
			return Security{}, ErrVersion
		}
		if !validAccountFloor(row) || version < 3 && !validLegacyAccount(row.Account) || key != accountKey(row.Account) {
			return Security{}, ErrInvalid
		}
		s.AccountFloors = append(s.AccountFloors, row)
	}
	for _, key := range root.ProviderFloors {
		var row ProviderFloor
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		if row.FormatVersion != FormatVersion {
			return Security{}, ErrVersion
		}
		if !validProviderFloor(row) || key != providerKey(row.Provider, row.UID) {
			return Security{}, ErrInvalid
		}
		s.ProviderFloors = append(s.ProviderFloors, row)
	}
	for _, key := range root.Sessions {
		var row SessionRecord
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		if row.FormatVersion != FormatVersion {
			return Security{}, ErrVersion
		}
		if !canonicalEpoch(own[key]) || !validSession(row, s.Epoch) || key != SessionKey(row.ID) {
			return Security{}, ErrInvalid
		}
		s.Sessions = append(s.Sessions, row)
	}
	for _, key := range root.TOTP {
		if seen[key] {
			return Security{}, ErrInvalid
		}
		seen[key] = true
		raw, ok := own[key]
		if !ok {
			return Security{}, ErrManifest
		}
		row, err := totp.DecodeRecord(replication.Entry{Key: key, Data: raw})
		if err != nil {
			return Security{}, err
		}
		s.TOTP = append(s.TOTP, row)
	}
	for _, key := range root.ExternalIdentities {
		var row ExternalIdentity
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		expected, err := ExternalIdentityKey(row.Provider, row.Issuer, row.Subject)
		if err != nil || expected != key || !requiredFields(own[key], "disabled", "groups", "groupsVerifiedAt") || !validExternalIdentity(row) {
			return Security{}, ErrInvalid
		}
		s.ExternalIdentities = append(s.ExternalIdentities, row)
	}
	for _, key := range root.ExternalSessions {
		var row ExternalSessionRecord
		if err := read(key, &row); err != nil {
			return Security{}, err
		}
		if key != ExternalSessionKey(row.ID) || !canonicalEpoch(own[key]) || !requiredFields(own[key], "revoked") || !validExternalSession(row, s.Epoch) {
			return Security{}, ErrInvalid
		}
		s.ExternalSessions = append(s.ExternalSessions, row)
	}
	if err := validateExternalRelations(s); err != nil {
		return Security{}, err
	}
	return s, nil
}

// ValidateEntry is the owner-level per-row validator for store/replica wiring.
// Cross-row completeness is enforced by DecodeSecurity/CaptureImage.
func ValidateEntry(section string, row replication.Entry) error {
	if section != "security" || !strings.HasPrefix(row.Key, ownerPrefix) {
		return nil
	}
	if len(row.Key) > 512 {
		return ErrBounds
	}
	if row.Key == RootKey {
		var r RootEntry
		if !strictOwner(row.Data, &r, MaxRootBytes) {
			return ErrInvalid
		}
		if r.FormatVersion != RootFormatVersion {
			return ErrVersion
		}
		if r.Epoch == 0 || !canonicalEpoch(row.Data) || r.GrantFloors == nil || r.AccountFloors == nil || r.ProviderFloors == nil || r.Sessions == nil || r.TOTP == nil || r.ExternalIdentities == nil || r.ExternalSessions == nil {
			return ErrManifest
		}
		return nil
	}
	if !strings.HasPrefix(row.Key, "auth/v1/") {
		return ErrVersion
	}
	if strings.HasPrefix(row.Key, "auth/v1/totp/") {
		return totp.ValidateEntry(section, row)
	}
	if len(row.Data) > MaxRowBytes {
		return ErrBounds
	}
	var header struct {
		FormatVersion int `json:"formatVersion"`
	}
	if !boundedJSON(row.Data, MaxRowBytes) || json.Unmarshal(row.Data, &header) != nil {
		return ErrInvalid
	}
	if header.FormatVersion != FormatVersion {
		return ErrVersion
	}
	switch {
	case strings.HasPrefix(row.Key, "auth/v1/external-"):
		return validateExternalEntry(row)
	case strings.HasPrefix(row.Key, "auth/v1/grant/"):
		var r GrantFloor
		if strictOwner(row.Data, &r, MaxRowBytes) && validGrantFloor(r) && row.Key == grantKey(r.Grant) {
			return nil
		}
	case strings.HasPrefix(row.Key, "auth/v1/account/"):
		var r AccountFloor
		if strictOwner(row.Data, &r, MaxRowBytes) && validAccountFloor(r) && row.Key == accountKey(r.Account) {
			return nil
		}
	case strings.HasPrefix(row.Key, "auth/v1/provider/"):
		var r ProviderFloor
		if strictOwner(row.Data, &r, MaxRowBytes) && validProviderFloor(r) && row.Key == providerKey(r.Provider, r.UID) {
			return nil
		}
	case strings.HasPrefix(row.Key, "auth/v1/session/"):
		var r SessionRecord
		if strictOwner(row.Data, &r, MaxRowBytes) && canonicalEpoch(row.Data) && validSession(r, ^uint64(0)) && row.Key == SessionKey(r.ID) {
			return nil
		}
	}
	return ErrInvalid
}
func canonicalEpoch(raw []byte) bool {
	var h map[string]json.RawMessage
	if json.Unmarshal(raw, &h) != nil {
		return false
	}
	var s string
	if json.Unmarshal(h["epoch"], &s) != nil {
		return false
	}
	n, e := strconv.ParseUint(s, 10, 64)
	return e == nil && strconv.FormatUint(n, 10) == s
}
func validAccount(a AccountRef) bool {
	return validLegacyAccount(a) || a.Kind == "ExternalIdentity" && a.Namespace == "" && hashPattern.MatchString(a.Name) && externalUIDPattern.MatchString(a.UID)
}
func validLegacyAccount(a AccountRef) bool {
	return (a.Kind == "LocalUser" && a.Namespace == "" || a.Kind == "ServiceAccount" && len(validation.IsDNS1123Label(a.Namespace)) == 0) && len(validation.IsDNS1123Subdomain(a.Name)) == 0 && a.UID != "" && len(a.UID) <= 512 && !strings.ContainsRune(a.UID, 0)
}
func validGrant(s string) bool {
	if s == "" || len(s) > 512 {
		return false
	}
	g, e := rbac.ParseGrant(s)
	return e == nil && g.Normalized().String() == s
}
func validGrantFloor(r GrantFloor) bool {
	return r.FormatVersion == FormatVersion && validGrant(r.Grant) && validTime(r.ValidAfter)
}
func validAccountFloor(r AccountFloor) bool {
	return r.FormatVersion == FormatVersion && validAccount(r.Account) && validTime(r.ValidAfter)
}
func validProviderFloor(r ProviderFloor) bool {
	return r.FormatVersion == FormatVersion && len(validation.IsDNS1123Subdomain(r.Provider)) == 0 && r.UID != "" && len(r.UID) <= 512 && !strings.ContainsRune(r.UID, 0) && validTime(r.ValidAfter)
}
func validTime(t time.Time) bool { return !t.IsZero() && t.Year() >= 1970 && t.Year() <= 9999 }
func validSession(r SessionRecord, epoch uint64) bool {
	if r.FormatVersion != FormatVersion || !sessionIDPattern.MatchString(r.ID) || !validLegacyAccount(r.Account) || r.Epoch == 0 || r.Epoch > epoch || r.Grants == nil || len(r.Grants) > MaxGrants || !hashPattern.MatchString(r.CSRFHash) || !validTime(r.IssuedAt) || !validTime(r.ExpiresAt) || !validTime(r.IdleExpiresAt) || !r.ExpiresAt.After(r.IssuedAt) || !r.IdleExpiresAt.After(r.IssuedAt) || r.IdleExpiresAt.After(r.ExpiresAt) {
		return false
	}
	if r.Account.Kind == "LocalUser" && (r.Provider != "local" || r.ProviderUID != "") || r.Account.Kind == "ServiceAccount" && (r.Provider != "kudurru" || r.ProviderUID != "") {
		return false
	}
	for i, g := range r.Grants {
		if !validGrant(g) || slices.Contains(r.Grants[:i], g) {
			return false
		}
	}
	return true
}
func grantKey(g string) string         { return "auth/v1/grant/" + hash(g) }
func accountKey(a AccountRef) string   { return "auth/v1/account/" + hash(a) }
func providerKey(p, uid string) string { return "auth/v1/provider/" + hash([]string{p, uid}) }

// SessionKey is deterministic; no credential secret forms a row key.
func SessionKey(id string) string { return "auth/v1/session/" + id }
