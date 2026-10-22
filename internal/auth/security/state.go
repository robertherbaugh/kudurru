// Package security commits the serving-auth owner's durable state through the
// caller's admitted SQLite command. It never authorizes a caller or opens a
// second transaction. Credentials, current grants and CSRF must be checked by
// the command before invoking these mutations, including on dedup replay.
package security

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	internalv1 "github.com/robertherbaugh/kudurru/api/internalkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ErrConflict requires a fresh owner read after another edit in this command.
var ErrConflict = errors.New("auth/security: owner state changed")

// Initialize is an explicit startup migration, never a read fallback. Existing
// owner rows must be complete; losing the root cannot erase revocation history.
// Other owners' rows are preserved. Follower startup must not invoke it.
func Initialize(ctx context.Context, tx *sqlite.CommandTx) error {
	if tx == nil {
		return servingauth.ErrInvalid
	}
	rows, err := readSecurityRows(ctx, tx)
	if err != nil {
		return err
	}
	initial := servingauth.EmptySecurity(1)
	legacyFactors := true
	if len(owned(rows)) != 0 {
		version, err := servingauth.SecurityRootVersion(rows)
		if err != nil {
			return err
		}
		switch version {
		case servingauth.RootFormatVersion:
			_, err := servingauth.DecodeSecurity(rows)
			return err
		case 3:
			initial, err = servingauth.UpgradeSecurityV3(rows)
			legacyFactors = false
		case 2:
			initial, err = servingauth.UpgradeSecurityV2(rows)
			legacyFactors = false // v2 already owns explicit durable replay rows.
		case 1:
			initial, err = servingauth.UpgradeSecurityV1(rows)
		default:
			return servingauth.ErrVersion
		}
		if err != nil {
			return err
		}
	}
	if legacyFactors {
		if err := rejectLegacyFactors(ctx, tx); err != nil {
			return err
		}
	}
	encoded, err := servingauth.EncodeSecurity(initial)
	if err != nil {
		return err
	}
	if err := validateCombined(rows, encoded); err != nil {
		return err
	}
	for _, row := range encoded {
		if err := tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	return nil
}

func rejectLegacyFactors(ctx context.Context, tx *sqlite.CommandTx) error {
	// Old seeds have no durable replay authority. Never adopt one or silently
	// clear it while installing an empty inventory, even for disabled accounts.
	accounts, err := tx.List(ctx, "LocalUser", store.ListOptions{Limit: servingauth.MaxObjects + 1})
	if err != nil {
		return err
	}
	if accounts.Continue != "" || len(accounts.Items) > servingauth.MaxObjects {
		return servingauth.ErrBounds
	}
	for _, object := range accounts.Items {
		user, ok := object.(*internalv1.LocalUser)
		if !ok || user == nil {
			return servingauth.ErrInvalid
		}
		if user.Spec.TOTPSecret != "" {
			return servingauth.ErrUnsupported
		}
	}
	return nil
}

// State is a transaction-bound edit. Its methods expose no mutable snapshot and
// cannot lower floors, reuse session IDs or remove unexpired enrollments.
type State struct {
	tx        *sqlite.CommandTx
	before    []replication.Entry
	otherRows int
	value     servingauth.Security
	changed   bool
	finished  bool
}

// Load captures a complete validated owner state in the admitted transaction.
func Load(ctx context.Context, tx *sqlite.CommandTx) (*State, error) {
	if tx == nil {
		return nil, servingauth.ErrInvalid
	}
	rows, err := readSecurityRows(ctx, tx)
	if err != nil {
		return nil, err
	}
	v, err := servingauth.DecodeSecurity(rows)
	if err != nil {
		return nil, err
	}
	before := owned(rows)
	return &State{tx: tx, before: before, otherRows: len(rows) - len(before), value: v}, nil
}

func owned(rows []replication.Entry) []replication.Entry {
	out := make([]replication.Entry, 0, len(rows))
	for _, row := range rows {
		if strings.HasPrefix(row.Key, "auth/") {
			out = append(out, replication.Entry{Key: row.Key, Data: bytes.Clone(row.Data)})
		}
	}
	slices.SortFunc(out, func(a, b replication.Entry) int { return strings.Compare(a.Key, b.Key) })
	return out
}

func (s *State) valid() error {
	if s == nil || s.tx == nil || s.finished {
		return sqlite.ErrTransactionEnded
	}
	return nil
}

func (s *State) capacity() error {
	if s.otherRows+len(s.value.GrantFloors)+len(s.value.AccountFloors)+len(s.value.ProviderFloors)+len(s.value.Sessions)+len(s.value.TOTP)+len(s.value.ExternalIdentities)+len(s.value.ExternalSessions)+len(s.value.BrowserAttempts) >= servingauth.MaxRows-1 {
		return servingauth.ErrBounds
	}
	return nil
}

// InvalidateGrant conservatively invalidates every older enrollment containing
// this normalized grant. Floors persist after deletion and later regrant.
func (s *State) InvalidateGrant(grant string, at time.Time) error {
	if err := s.valid(); err != nil {
		return err
	}
	g, err := rbac.ParseGrant(grant)
	if err != nil || at.IsZero() {
		return servingauth.ErrInvalid
	}
	grant = g.Normalized().String()
	for i, f := range s.value.GrantFloors {
		if f.Grant == grant {
			if at.After(f.ValidAfter) {
				s.value.GrantFloors[i].ValidAfter = at.UTC()
				s.changed = true
			}
			return nil
		}
	}
	if err := s.capacity(); err != nil {
		return err
	}
	s.value.GrantFloors = append(s.value.GrantFloors, servingauth.GrantFloor{FormatVersion: servingauth.FormatVersion, Grant: grant, ValidAfter: at.UTC()})
	s.changed = true
	return nil
}

// InvalidateAccount raises the floor for one exact account incarnation.
func (s *State) InvalidateAccount(account servingauth.AccountRef, at time.Time) error {
	if err := s.valid(); err != nil {
		return err
	}
	if at.IsZero() {
		return servingauth.ErrInvalid
	}
	for i, f := range s.value.AccountFloors {
		if f.Account == account {
			if at.After(f.ValidAfter) {
				s.value.AccountFloors[i].ValidAfter = at.UTC()
				s.changed = true
			}
			return nil
		}
	}
	if err := s.capacity(); err != nil {
		return err
	}
	s.value.AccountFloors = append(s.value.AccountFloors, servingauth.AccountFloor{FormatVersion: servingauth.FormatVersion, Account: account, ValidAfter: at.UTC()})
	s.changed = true
	return nil
}

// InvalidateProvider raises the floor for one exact provider incarnation.
func (s *State) InvalidateProvider(provider, uid string, at time.Time) error {
	if err := s.valid(); err != nil {
		return err
	}
	if at.IsZero() {
		return servingauth.ErrInvalid
	}
	for i, f := range s.value.ProviderFloors {
		if f.Provider == provider && f.UID == uid {
			if at.After(f.ValidAfter) {
				s.value.ProviderFloors[i].ValidAfter = at.UTC()
				s.changed = true
			}
			return nil
		}
	}
	if err := s.capacity(); err != nil {
		return err
	}
	s.value.ProviderFloors = append(s.value.ProviderFloors, servingauth.ProviderFloor{FormatVersion: servingauth.FormatVersion, Provider: provider, UID: uid, ValidAfter: at.UTC()})
	s.changed = true
	return nil
}

// Enroll accepts only a freshly authenticated account/grant decision from this
// command. The row becomes usable only after the enclosing transaction commits.
// The returned value is detached and carries the upcoming owner epoch.
func (s *State) Enroll(record servingauth.SessionRecord) (servingauth.SessionRecord, error) {
	if err := s.valid(); err != nil {
		return servingauth.SessionRecord{}, err
	}
	if s.value.Epoch == math.MaxUint64 || record.Revoked {
		return servingauth.SessionRecord{}, servingauth.ErrInvalid
	}
	if err := s.capacity(); err != nil {
		return servingauth.SessionRecord{}, err
	}
	for _, old := range s.value.Sessions {
		if old.ID == record.ID {
			return servingauth.SessionRecord{}, ErrConflict
		}
	}
	for _, f := range s.value.AccountFloors {
		if f.Account == record.Account && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.SessionRecord{}, servingauth.ErrCredentials
		}
	}
	for _, f := range s.value.ProviderFloors {
		if f.Provider == record.Provider && f.UID == record.ProviderUID && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.SessionRecord{}, servingauth.ErrCredentials
		}
	}
	for _, f := range s.value.GrantFloors {
		if slices.Contains(record.Grants, f.Grant) && !record.IssuedAt.After(f.ValidAfter) {
			return servingauth.SessionRecord{}, servingauth.ErrCredentials
		}
	}
	record.FormatVersion = servingauth.FormatVersion
	record.Epoch = s.value.Epoch + 1
	record.Grants = slices.Clone(record.Grants)
	s.value.Sessions = append(s.value.Sessions, record)
	s.changed = true
	record.Grants = slices.Clone(record.Grants)
	return record, nil
}

// Revoke is an idempotent command. It retains the enrollment until its absolute
// expiry so a retry cannot recreate it under the same identifier.
func (s *State) Revoke(id string) error {
	if err := s.valid(); err != nil {
		return err
	}
	for i, row := range s.value.Sessions {
		if row.ID == id && !row.Revoked {
			s.value.Sessions[i].Revoked = true
			s.changed = true
		}
	}
	for i, row := range s.value.ExternalSessions {
		if row.ID == id && !row.Revoked {
			s.value.ExternalSessions[i].Revoked = true
			s.changed = true
		}
	}
	return nil
}

// CollectExpired removes enrollments and pending browser attempts whose absolute
// lifetime has ended. Idle-expired and revoked sessions remain until then;
// revocation floors remain. Missing attempts can never be consumed.
func (s *State) CollectExpired(now time.Time) error {
	if err := s.valid(); err != nil {
		return err
	}
	if now.IsZero() {
		return servingauth.ErrInvalid
	}
	n := len(s.value.Sessions)
	s.value.Sessions = slices.DeleteFunc(s.value.Sessions, func(r servingauth.SessionRecord) bool { return !now.Before(r.ExpiresAt) })
	s.changed = s.changed || len(s.value.Sessions) != n
	n = len(s.value.ExternalSessions)
	s.value.ExternalSessions = slices.DeleteFunc(s.value.ExternalSessions, func(r servingauth.ExternalSessionRecord) bool { return !now.Before(r.ExpiresAt) })
	s.changed = s.changed || len(s.value.ExternalSessions) != n
	n = len(s.value.BrowserAttempts)
	s.value.BrowserAttempts = slices.DeleteFunc(s.value.BrowserAttempts, func(r servingauth.BrowserAttempt) bool { return !now.Before(r.ExpiresAt) })
	s.changed = s.changed || len(s.value.BrowserAttempts) != n
	return nil
}

// Commit stages all changed rows plus the explicit root manifest in the SAME
// command as its object mutations. Concurrent edits inside that command must
// reload, never overwrite a newer floor/enrollment. Other owners are untouched.
func (s *State) Commit(ctx context.Context) error {
	if err := s.valid(); err != nil {
		return err
	}
	s.finished = true
	current, err := readSecurityRows(ctx, s.tx)
	if err != nil {
		return err
	}
	if !slices.EqualFunc(s.before, owned(current), func(a, b replication.Entry) bool { return a.Key == b.Key && bytes.Equal(a.Data, b.Data) }) {
		return ErrConflict
	}
	if !s.changed {
		_, err := servingauth.DecodeSecurity(current)
		return err
	}
	if s.value.Epoch == math.MaxUint64 {
		return servingauth.ErrBounds
	}
	s.value.Epoch++
	next, err := servingauth.EncodeSecurity(s.value)
	if err != nil {
		return err
	}
	if err := validateCombined(current, next); err != nil {
		return err
	}
	keep := make(map[string]bool, len(next))
	for _, row := range next {
		keep[row.Key] = true
		if err := s.tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	for _, row := range s.before {
		if !keep[row.Key] {
			if err := s.tx.DeleteSection(ctx, "security", row.Key); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateCombined accounts for every owner in the shared section, including
// changes staged after Load. Replacing only our rows preserves other owners.
func validateCombined(current, next []replication.Entry) error {
	combined := append([]replication.Entry(nil), next...)
	for _, row := range current {
		if !strings.HasPrefix(row.Key, "auth/") {
			combined = append(combined, row)
		}
	}
	_, err := servingauth.DecodeSecurity(combined)
	return err
}
