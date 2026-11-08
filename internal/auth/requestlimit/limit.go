// Package requestlimit charges bounded, non-granting browser request budgets.
// It runs before the first authentication capture, never as a continuation of
// an already captured credential or provider operation.
package requestlimit

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/loginlimit"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

const (
	// Prefix, Key and Section identify one optional complete replicated owner.
	Prefix  = "browser-admission/"
	Key     = Prefix + "v1/state"
	Section = "runtime"
	// MaxBytes bounds the document before decoding or staging it.
	MaxBytes = 512 << 10
	// MaxKeys bounds live entries across all request lanes.
	MaxKeys = 4096
	// Lifetime bounds one original request, including admission and response.
	Lifetime = 30 * time.Second
	window   = time.Minute
)

// Lane identifies a fixed request policy, not an authentication result.
type Lane string

const (
	// Start, Callback and CLI have independent, fixed per-prefix policies.
	Start    Lane = "start"
	Callback Lane = "callback"
	CLI      Lane = "cli"
)

func (l Lane) maximum() int {
	switch l {
	case Start, CLI:
		return 5
	case Callback:
		return 10
	default:
		return 0
	}
}

// ErrState refuses malformed history, unknown policies and clock rollback.
var ErrState = errors.New("requestlimit: invalid shared admission state")

// ErrCapacity refuses a new charge rather than evicting live history.
var ErrCapacity = errors.New("requestlimit: shared admission capacity exhausted")

type entry struct {
	Lane  Lane      `json:"lane"`
	Used  int       `json:"used"`
	Last  time.Time `json:"last"`
	Until time.Time `json:"until"`
}

type document struct {
	Version int              `json:"version"`
	Salt    string           `json:"salt"`
	At      time.Time        `json:"at"`
	Entries map[string]entry `json:"entries"`
}

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(b) == s
}

func decode(raw []byte) (*document, error) {
	if len(raw) == 0 || len(raw) > MaxBytes {
		return nil, ErrState
	}
	d := new(document)
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(d) != nil || !errors.Is(dec.Decode(new(any)), io.EOF) {
		return nil, ErrState
	}
	if d.Version != 1 || !validHash(d.Salt) || d.At.IsZero() || d.Entries == nil || len(d.Entries) > MaxKeys {
		return nil, ErrState
	}
	for k, e := range d.Entries {
		if !validHash(k) || e.Lane.maximum() == 0 || e.Used < 0 || e.Used >= e.Lane.maximum() || e.Last.IsZero() || e.Last.After(d.At) || (e.Used == 0) != !e.Until.IsZero() || (!e.Until.IsZero() && !e.Until.Equal(e.Last.Add(window))) {
			return nil, ErrState
		}
	}
	return d, nil
}

func charge(raw []byte, lane Lane, client string, now time.Time) ([]byte, error) {
	if lane.maximum() == 0 || client == "" || len(client) > 128 || now.IsZero() {
		return nil, ErrState
	}
	var d *document
	if raw == nil {
		var salt [32]byte
		_, _ = rand.Read(salt[:])
		d = &document{Version: 1, Salt: hex.EncodeToString(salt[:]), At: now, Entries: map[string]entry{}}
	} else {
		var err error
		d, err = decode(raw)
		if err != nil {
			return nil, err
		}
	}
	if now.Before(d.At) {
		return nil, ErrState
	}
	// Reclaim expired entries only; capacity never evicts a live lockout.
	for key, e := range d.Entries {
		if !now.Before(e.Until) && now.Sub(e.Last) > window {
			delete(d.Entries, key)
		}
	}
	mac := hmac.New(sha256.New, []byte(d.Salt))
	_, _ = mac.Write([]byte(string(lane) + "\x00" + client))
	key := hex.EncodeToString(mac.Sum(nil))
	e, found := d.Entries[key]
	if !found {
		if len(d.Entries) >= MaxKeys {
			return nil, ErrCapacity
		}
		e.Lane = lane
	}
	if e.Lane != lane {
		return nil, ErrState
	}
	if now.Before(e.Until) {
		return nil, &loginlimit.Locked{RetryAfter: e.Until.Sub(now)}
	}
	if now.Sub(e.Last) > window {
		e.Used = 0
	}
	e.Used++
	e.Last = now
	// A completed lockout is historical; never extend it on a denied request.
	e.Until = time.Time{}
	if e.Used == lane.maximum() {
		e.Used = 0
		e.Until = now.Add(window)
	}
	d.Entries[key], d.At = e, now
	next, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	if len(next) > MaxBytes {
		return nil, ErrCapacity
	}
	return next, nil
}

// Charge stages exactly one request charge in the supplied original command.
// It grants no credential, provider, read or write authority. The caller must
// return every error, confirm actual commit, and retain the original context
// and writer before dispatching the request for its FIRST authentication capture.
func Charge(ctx context.Context, tx *sqlite.CommandTx, lane Lane, client string, clock func() time.Time) error {
	if ctx == nil || tx == nil {
		return ErrState
	}
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > Lifetime {
		return ErrState
	}
	if clock == nil {
		clock = time.Now
	}
	at := clock().UTC()
	bound := at.Add(Lifetime)
	if deadline.Before(bound) {
		bound = deadline
	}
	if !bound.After(at) {
		return ErrState
	}
	if err := tx.CheckSectionPrefixBounds(ctx, Section, Prefix, 1, MaxBytes); err != nil {
		return err
	}
	rows, err := tx.ListSectionPrefix(ctx, Section, Prefix)
	if err != nil {
		return err
	}
	var raw []byte
	if len(rows) != 0 {
		if len(rows) != 1 || rows[0].Key != Key || len(rows[0].Data) == 0 {
			return ErrState
		}
		raw = rows[0].Data
	}
	next, err := charge(raw, lane, client, at)
	if err != nil {
		return err
	}
	if err := tx.PutSection(ctx, Section, replication.Entry{Key: Key, Data: next}); err != nil {
		return err
	}
	return tx.RequireCommitCheck(func(final context.Context) error {
		if err := final.Err(); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		now := clock().UTC()
		if now.Before(at) || !now.Before(bound) {
			return ErrState
		}
		return nil
	})
}

// ValidateEntry validates source time without reinterpretation at a follower.
// Legacy absence is permitted; privileged removal explicitly resets the budget.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	if row.Key != Key {
		return ErrState
	}
	_, err := decode(row.Data)
	return err
}

// ValidateImage rejects malformed or ambiguous owner rows without source-time
// reinterpretation. Ordinary legacy absence does not grant authentication.
func ValidateImage(image *replication.Image) error {
	if image == nil || !image.Complete {
		return ErrState
	}
	count := 0
	for _, row := range image.Payload.Runtime {
		if !strings.HasPrefix(row.Key, Prefix) {
			continue
		}
		count++
		if count > 1 {
			return ErrState
		}
		if err := ValidateEntry(Section, row); err != nil {
			return err
		}
	}
	return nil
}
