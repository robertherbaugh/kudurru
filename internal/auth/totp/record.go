package totp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"reflect"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Record is detached durable replay state; it contains no seed.
type Record struct {
	FormatVersion    int     `json:"formatVersion"`
	Account          Account `json:"account"`
	SeedVersion      uint64  `json:"seedVersion,string"`
	SeedID           string  `json:"seedID"`
	Enabled          bool    `json:"enabled"`
	HasConsumed      bool    `json:"hasConsumed"`
	LastConsumedStep uint64  `json:"lastConsumedStep,string"`
}

// Key identifies an account incarnation independently of seed/key rotation.
func Key(a Account) string {
	if !validAccount(a) {
		return ""
	}
	raw, _ := json.Marshal([]string{"LocalUser", "", a.Name, a.UID})
	digest := sha256.Sum256(raw)
	return "auth/v1/totp/" + hex.EncodeToString(digest[:])
}

// EncodeRecord validates and returns a deterministic owner row.
func EncodeRecord(r Record) (replication.Entry, error) {
	if !validRecord(r) {
		return replication.Entry{}, ErrInvalid
	}
	raw, err := json.Marshal(r)
	if err != nil || len(raw) > MaxRecordBytes {
		return replication.Entry{}, ErrBounds
	}
	return replication.Entry{Key: Key(r.Account), Data: raw}, nil
}

// DecodeRecord rejects unsupported/malformed/case-aliased or oversized rows.
func DecodeRecord(row replication.Entry) (Record, error) {
	if len(row.Key) > 512 {
		return Record{}, ErrBounds
	}
	var r Record
	if err := decode(row.Data, &r, MaxRecordBytes); err != nil {
		return Record{}, err
	}
	if r.FormatVersion != FormatVersion {
		return Record{}, ErrVersion
	}
	if !validRecord(r) || row.Key != Key(r.Account) {
		return Record{}, ErrInvalid
	}
	return r, nil
}

// ValidateEntry validates only TOTP-owned rows; root composes other owners.
func ValidateEntry(section string, row replication.Entry) error {
	if section != "security" || !strings.HasPrefix(row.Key, "auth/v1/totp/") {
		return nil
	}
	_, err := DecodeRecord(row)
	return err
}
func validRecord(r Record) bool {
	return r.FormatVersion == FormatVersion && validAccount(r.Account) && r.SeedVersion != 0 && validID(r.SeedID) && (r.HasConsumed || r.LastConsumedStep == 0)
}

// Advance rejects wrong/disabled seed identity and already consumed steps.
// It performs no write and confers no authority outside the actual command.
func Advance(r Record, s *Seed, step uint64) (Record, error) {
	if !validRecord(r) || s == nil || !r.Enabled || r.Account != s.account || r.SeedVersion != s.version || r.SeedID != s.id {
		return Record{}, ErrProof
	}
	if r.HasConsumed && step <= r.LastConsumedStep {
		return Record{}, ErrReplay
	}
	r.HasConsumed, r.LastConsumedStep = true, step
	return r, nil
}

// decode checks caps and exact required field presence before typed allocation.
// This schema has only fixed nested objects, integers, booleans and strings.
func decode(raw []byte, into any, capBytes int) error {
	if len(raw) > capBytes {
		return ErrBounds
	}
	if len(raw) == 0 {
		return ErrInvalid
	}
	if !fields(raw, reflect.TypeOf(into).Elem()) {
		return ErrInvalid
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(into) != nil {
		return ErrInvalid
	}
	return nil
}
func fields(raw []byte, typ reflect.Type) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return false
	}
	seen := map[string]bool{}
	for d.More() {
		token, err := d.Token()
		name, ok := token.(string)
		if err != nil || !ok || seen[name] {
			return false
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return false
		}
		found := false
		for i := range typ.NumField() {
			f := typ.Field(i)
			if strings.Split(f.Tag.Get("json"), ",")[0] != name {
				continue
			}
			found = true
			if f.Type == reflect.TypeFor[Account]() {
				if !fields(value, f.Type) {
					return false
				}
			} else if f.Type.Kind() == reflect.Uint64 {
				if !canonicalUint(value) {
					return false
				}
			} else {
				v, err := json.NewDecoder(bytes.NewReader(value)).Token()
				if err != nil || v == nil {
					return false
				}
				if _, ok := v.(json.Delim); ok {
					return false
				}
			}
			break
		}
		if !found {
			return false
		}
	}
	if len(seen) != typ.NumField() {
		return false
	}
	if _, err = d.Token(); err != nil {
		return false
	}
	_, err = d.Token()
	return err == io.EOF
}
