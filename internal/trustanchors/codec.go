package trustanchors

import (
	"bytes"
	"encoding/json"
	"io"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/miekg/dns"
)

// Encode validates and serializes one bounded complete owner. It does not
// initialize missing history, prune records or change observation timestamps.
// Serialization is not a durable CAS receipt and cannot accept a checkpoint.
func Encode(s State) ([]byte, error) {
	if err := validate(s.doc); err != nil {
		return nil, err
	}
	b, err := json.Marshal(s.doc)
	if err != nil {
		return nil, ErrInvalid
	}
	if len(b) > MaxStateBytes {
		return nil, ErrBounds
	}
	return b, nil
}

// Decode rejects duplicate/unknown/omitted/null fields, aliases, trailing data,
// oversized documents and invalid complete history. It performs no trust reset.
// The caller must independently establish this document's accepted durable
// source; decoding a proposed transition cannot turn it into committed progress.
func Decode(b []byte) (State, error) {
	if len(b) == 0 || len(b) > MaxStateBytes {
		return State{}, ErrBounds
	}
	wire, err := canonicalJSON(b)
	if err != nil {
		return State{}, err
	}
	var doc Snapshot
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(&doc) != nil {
		return State{}, ErrInvalid
	}
	s := State{doc: doc}
	encoded, err := Encode(s)
	if err != nil {
		return State{}, err
	}
	typed, err := canonicalJSON(encoded)
	if err != nil || !bytes.Equal(wire, typed) {
		return State{}, ErrInvalid
	}
	return s, nil
}

func validLabel(s string) bool {
	if s == "" || len(s) > 256 || !utf8.ValidString(s) || strings.TrimSpace(s) != s {
		return false
	}
	for _, c := range s {
		if c < 0x20 || c == 0x7f {
			return false
		}
	}
	return true
}

func validate(d Snapshot) error {
	if (d.Format != FormatVersion && d.Format != RecoveryFormatVersion) || !validLabel(d.ClusterID) || !validLabel(d.PolicyID) || !validDigest(d.SeedDigest) || d.Generation == 0 || d.Keys == nil || len(d.Keys) == 0 || len(d.Keys) > MaxLiveKeys+MaxTombstones || len(d.Seeds) == 0 || len(d.Seeds) > MaxLiveKeys || !slices.IsSorted(d.Seeds) || digest([]byte(strings.Join(d.Seeds, "\n"))) != d.SeedDigest {
		return ErrInvalid
	}
	seeds := make([]*dns.DS, 0, len(d.Seeds))
	for i, text := range d.Seeds {
		if i > 0 && d.Seeds[i-1] == text {
			return ErrInvalid
		}
		seed, err := parseSeed(text)
		if err != nil {
			return err
		}
		seeds = append(seeds, seed)
	}
	recoverySeeds, err := validateRecoveries(d)
	if err != nil {
		return err
	}
	seeds = append(seeds, recoverySeeds...)
	if !validSession(d.Checkpoint.Session) || d.Checkpoint.Tick < 0 || !validTime(d.Checkpoint.Wall) {
		return ErrClock
	}
	active, live, tombstones := 0, 0, 0
	byID := make(map[string]Key, len(d.Keys))
	for i, k := range d.Keys {
		if !validDigest(k.ID) || (i > 0 && d.Keys[i-1].ID >= k.ID) || len(k.DNSKEY) > 8192 || k.Support == nil || len(k.Support) > MaxLiveKeys || !slices.IsSorted(k.Support) {
			return ErrInvalid
		}
		if !validTime(k.FirstSeen) || !validTime(k.LastSeen) || k.LastSeen.Before(k.FirstSeen) || k.LastSeen.After(d.Checkpoint.Wall) || k.Elapsed < 0 || k.Elapsed > holdDown(k.OriginalTTL) {
			return ErrInvalid
		}
		for n, id := range k.Support {
			if !validDigest(id) || (n > 0 && k.Support[n-1] == id) || id == k.ID {
				return ErrInvalid
			}
		}
		rr, err := dns.NewRR(k.DNSKEY)
		if err != nil {
			return ErrInvalid
		}
		key, ok := rr.(*dns.DNSKEY)
		if !ok || !eligible(key) || key.Flags & ^uint16(dns.ZONE|dns.SEP|dns.REVOKE) != 0 {
			return ErrInvalid
		}
		id, canonical, err := canonicalKey(key)
		if err != nil || id != k.ID || canonical != k.DNSKEY {
			return ErrInvalid
		}
		if len(k.Support) == 0 && !seeded(key, seeds) {
			return ErrInvalid
		}
		if k.AcceptedAt.IsZero() {
			if trusted(k) {
				return ErrInvalid
			}
		} else {
			if !validTime(k.AcceptedAt) || k.AcceptedAt.Before(k.FirstSeen) || k.AcceptedAt.After(k.LastSeen) || k.Status == AddPending {
				return ErrInvalid
			}
			if len(k.Support) > 0 && (k.Elapsed != holdDown(k.OriginalTTL) || !k.AcceptedAt.After(k.FirstSeen.Add(holdDown(k.OriginalTTL)))) {
				return ErrInvalid
			}
		}
		if k.Status == Revoked {
			tombstones++
			if key.Flags&dns.REVOKE == 0 || !validTime(k.RevokedAt) || k.RevokedAt.Before(k.FirstSeen) || k.RevokedAt.After(d.Checkpoint.Wall) || !k.MissingSince.IsZero() {
				return ErrInvalid
			}
		} else {
			live++
			if key.Flags&dns.REVOKE != 0 || !k.RevokedAt.IsZero() {
				return ErrInvalid
			}
			switch k.Status {
			case AddPending:
				if len(k.Support) == 0 || !k.MissingSince.IsZero() {
					return ErrInvalid
				}
			case Valid:
				active++
				if !k.MissingSince.IsZero() {
					return ErrInvalid
				}
			case Missing:
				active++
				if !validTime(k.MissingSince) || k.MissingSince.Before(k.FirstSeen) || k.MissingSince.After(d.Checkpoint.Wall) {
					return ErrInvalid
				}
			default:
				return ErrInvalid
			}
		}
		byID[k.ID] = k
	}
	if live > MaxLiveKeys || tombstones > MaxTombstones {
		return ErrBounds
	}
	if d.Quarantined != (active == 0) {
		return ErrInvalid
	}
	for _, k := range d.Keys {
		alive := false
		for _, id := range k.Support {
			other, ok := byID[id]
			if !ok || other.AcceptedAt.IsZero() || other.AcceptedAt.After(k.FirstSeen) {
				return ErrInvalid
			}
			alive = alive || trusted(other)
		}
		if k.Status == AddPending && !alive {
			return ErrInvalid
		}
	}
	s := d.Schedule
	if !validTime(s.ObservedAt) || s.ObservedAt.After(d.Checkpoint.Wall) || !validTime(s.SignatureUntil) || !validTime(s.NextRefresh) {
		return ErrInvalid
	}
	expected, err := schedule(s.ObservedAt, s.OriginalTTL, s.SignatureUntil)
	if err != nil || expected != s {
		return ErrInvalid
	}
	return nil
}

func canonicalJSON(b []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := jsonValue(d, 16)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrInvalid
	}
	if v == nil {
		return nil, ErrInvalid
	}
	return json.Marshal(v)
}

func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth == 0 {
		return nil, ErrBounds
	}
	t, err := d.Token()
	if err != nil {
		return nil, ErrInvalid
	}
	delim, composite := t.(json.Delim)
	if !composite {
		return t, nil
	}
	switch delim {
	case '{':
		m := map[string]any{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return nil, ErrInvalid
			}
			if _, exists := m[name]; exists {
				return nil, ErrInvalid
			}
			v, err := jsonValue(d, depth-1)
			if err != nil {
				return nil, err
			}
			m[name] = v
		}
		if end, err := d.Token(); err != nil || end != json.Delim('}') {
			return nil, ErrInvalid
		}
		return m, nil
	case '[':
		a := []any{}
		for d.More() {
			v, err := jsonValue(d, depth-1)
			if err != nil {
				return nil, err
			}
			a = append(a, v)
		}
		if end, err := d.Token(); err != nil || end != json.Delim(']') {
			return nil, ErrInvalid
		}
		return a, nil
	}
	return nil, ErrInvalid
}

// RetryDue schedules a NEW bounded protocol attempt after an acquisition
// failure. It does not modify State or retry a refused state mutation.
func (s State) RetryDue(failed Sample) (time.Time, error) {
	if _, err := s.Version(); err != nil {
		return time.Time{}, err
	}
	if !failed.valid() || failed.wall.Before(s.doc.Checkpoint.Wall) {
		return time.Time{}, ErrClock
	}
	return failed.wall.Add(s.doc.Schedule.RetryInterval), nil
}
