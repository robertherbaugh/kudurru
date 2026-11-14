// Package sqliteadapter stages shared password counters in the caller's original
// admitted command. It never opens a command, recaptures a lease or authenticates.
package sqliteadapter

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const (
	// Prefix is separate from auth enrollment and provider-owner manifests.
	Prefix = "password-admission/"
	// Key owns the optional legacy-compatible complete counter document.
	Key = Prefix + "v1/state"
	// Section is carried by complete replication images, never safe object export.
	Section = "runtime"
)

func policies() (loginlimit.Policy, loginlimit.Policy) {
	return loginlimit.Policy{Failures: 5, Window: 5 * time.Minute, Lockout: 5 * time.Minute}, loginlimit.Policy{Failures: 10, Window: 15 * time.Minute, Lockout: 15 * time.Minute}
}

// New requires the actual active command; its backend cannot outlive that command.
func New(ctx context.Context, tx *sqlite.CommandTx, now func() time.Time) (*loginlimit.Limiter, error) {
	if tx == nil || ctx == nil {
		return nil, loginlimit.ErrState
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return nil, loginlimit.ErrState
	}
	if now == nil {
		now = time.Now
	}
	start := now().UTC()
	bound := start.Add(loginlimit.Lifetime)
	if deadline.Before(bound) {
		bound = deadline.UTC()
	}
	if !bound.After(start) {
		return nil, loginlimit.ErrState
	}
	ip, user := policies()
	return loginlimit.New(backend{tx: tx, original: ctx, now: now, bound: bound}, ip, user, now)
}

type backend struct {
	tx       *sqlite.CommandTx
	original context.Context
	now      func() time.Time
	bound    time.Time
}

func (b backend) UpdateLoginLimit(ctx context.Context, change func([]byte) ([]byte, error)) error {
	if err := b.tx.CheckSectionPrefixBounds(ctx, Section, Prefix, 1, loginlimit.MaxBytes); err != nil {
		return err
	}
	rows, err := b.tx.ListSectionPrefix(ctx, Section, Prefix)
	if err != nil {
		return err
	}
	var raw []byte
	if len(rows) > 0 {
		if len(rows) != 1 || rows[0].Key != Key {
			return loginlimit.ErrState
		}
		raw = rows[0].Data
	}
	next, err := change(raw)
	if err != nil {
		return err
	}
	row := replication.Entry{Key: Key, Data: next}
	if err := ValidateEntry(Section, row); err != nil {
		return err
	}
	if err := b.tx.PutSection(ctx, Section, row); err != nil {
		return err
	}
	var header struct{ At time.Time }
	if err := json.Unmarshal(next, &header); err != nil {
		return err
	}
	return b.tx.RequireCommitCheck(func(final context.Context) error {
		if err := final.Err(); err != nil {
			return err
		}
		if err := b.original.Err(); err != nil {
			return err
		}
		now := b.now().UTC()
		if now.Before(header.At) || !now.Before(b.bound) {
			return loginlimit.ErrState
		}
		return nil
	})
}

// ValidateEntry rejects malformed state and unknown versions without inventing
// absent history. A privileged removal explicitly resets this operational limit.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	if row.Key != Key {
		return loginlimit.ErrState
	}
	ip, user := policies()
	return loginlimit.Validate(row.Data, ip, user)
}

// ValidateImage checks any supplied owner state; legacy images may omit it.
func ValidateImage(image *replication.Image) error {
	if image == nil || !image.Complete {
		return loginlimit.ErrState
	}
	count := 0
	for _, row := range image.Payload.Runtime {
		if !strings.HasPrefix(row.Key, Prefix) {
			continue
		}
		count++
		if count > 1 {
			return loginlimit.ErrState
		}
		if err := ValidateEntry(Section, row); err != nil {
			return err
		}
	}
	return nil
}
