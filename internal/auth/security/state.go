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

	"github.com/robertherbaugh/kudurru/internal/rbac"
	"github.com/robertherbaugh/kudurru/internal/servingauth"
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
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return err
	}
	if len(owned(rows)) != 0 {
		_, err := servingauth.DecodeSecurity(rows)
		return err
	}
	initial, err := servingauth.EncodeSecurity(servingauth.EmptySecurity(1))
	if err != nil {
		return err
	}
	for _, row := range initial {
		if err := tx.PutSection(ctx, "security", row); err != nil {
			return err
		}
	}
	return nil
}

// State is a transaction-bound edit. Its methods expose no mutable snapshot and
// cannot lower floors, reuse session IDs or remove unexpired enrollments.
type State struct {
	tx       *sqlite.CommandTx
	before   []replication.Entry
	value    servingauth.Security
	changed  bool
	finished bool
}

// Load captures a complete validated owner state in the admitted transaction.
func Load(ctx context.Context, tx *sqlite.CommandTx) (*State, error) {
	if tx == nil {
		return nil, servingauth.ErrInvalid
	}
	rows, err := tx.ListSections(ctx, "security")
	if err != nil {
		return nil, err
	}
	v, err := servingauth.DecodeSecurity(rows)
	if err != nil {
		return nil, err
	}
	return &State{tx: tx, before: owned(rows), value: v}, nil
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
	if len(s.value.GrantFloors)+len(s.value.AccountFloors)+len(s.value.ProviderFloors)+len(s.value.Sessions) >= servingauth.MaxRows-1 {
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
	return nil
}

// CollectExpired removes only enrollments whose absolute lifetime has ended.
// Idle-expired and revoked sessions remain until then; revocation floors remain.
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
	current, err := s.tx.ListSections(ctx, "security")
	if err != nil {
		return err
	}
	if !slices.EqualFunc(s.before, owned(current), func(a, b replication.Entry) bool { return a.Key == b.Key && bytes.Equal(a.Data, b.Data) }) {
		return ErrConflict
	}
	if !s.changed {
		return nil
	}
	if s.value.Epoch == math.MaxUint64 {
		return servingauth.ErrBounds
	}
	s.value.Epoch++
	next, err := servingauth.EncodeSecurity(s.value)
	if err != nil {
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
