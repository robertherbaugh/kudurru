package browseroidc

import (
	"bytes"
	"encoding/json"
	"io"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/session"
)

// Codec holds immutable purpose-separated current/previous cluster keys.
type Codec struct{ sealer *session.Sealer }

func (*Codec) String() string { return "private browser preauthentication codec" }

// GoString prevents diagnostic key disclosure.
func (c *Codec) GoString() string { return c.String() }

// MarshalJSON prevents accidental key serialization.
func (*Codec) MarshalJSON() ([]byte, error) { return []byte(`"private codec"`), nil }

// NewCodec derives a distinct preauthentication purpose. Legacy Stack cookies
// and enrolled session cookies cannot be opened with this codec.
func NewCodec(current, previous []byte) (*Codec, error) {
	s, err := session.NewSealer(Purpose, current, previous)
	if err != nil {
		return nil, ErrInvalid
	}
	return &Codec{sealer: s}, nil
}

// Seal authenticates a valid pending payload; it does not admit a durable row.
func (c *Codec) Seal(s State, now time.Time) (string, error) {
	if c == nil || c.sealer == nil {
		return "", ErrInvalid
	}
	if err := s.ValidAt(now); err != nil {
		return "", err
	}
	raw, _ := json.Marshal(s.value)
	defer clear(raw)
	if len(raw) > maxPlainBytes || session.SealedLen(len(raw)) > MaxCookieBytes {
		return "", ErrBounds
	}
	return c.sealer.Seal(raw), nil
}

// Open checks caps before decoding and accepts only the exact canonical format.
// The caller still must require and atomically consume the matching pending row.
func (c *Codec) Open(cookie string, now time.Time) (State, error) {
	if c == nil || c.sealer == nil {
		return State{}, ErrInvalid
	}
	if len(cookie) > MaxCookieBytes {
		return State{}, ErrBounds
	}
	raw, _, err := c.sealer.Open(cookie)
	if err != nil {
		return State{}, ErrInvalid
	}
	defer clear(raw)
	if len(raw) > maxPlainBytes {
		return State{}, ErrBounds
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	var p payload
	if d.Decode(&p) != nil || d.Decode(new(any)) != io.EOF {
		return State{}, ErrInvalid
	}
	canonical, _ := json.Marshal(p)
	defer clear(canonical)
	// This rejects duplicates, missing fields, alternate time encodings, and
	// noncanonical payloads without allocating an attacker-sized key inventory.
	if !bytes.Equal(raw, canonical) {
		return State{}, ErrInvalid
	}
	s := State{value: p}
	if err := s.ValidAt(now); err != nil {
		return State{}, err
	}
	return s, nil
}
