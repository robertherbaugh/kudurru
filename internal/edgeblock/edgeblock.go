// Package edgeblock is the bounded set of operator-initiated edge blocks
// (D388): a client prefix that every dnsdist edge drops until an absolute
// expiry. It owns validation, the versioned document and the pure changes.
// It does no I/O; the store drivers persist the document and the edge
// supervisors apply what backends publish.
package edgeblock

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"slices"
	"strings"
	"time"
	"unicode/utf8"
)

// Bounds of one block and of the whole set.
const (
	// MaxBlocks bounds the stored set after expired entries are pruned.
	MaxBlocks = 256
	// MinDuration and MaxDuration bound one block's lifetime.
	MinDuration = time.Minute
	MaxDuration = 7 * 24 * time.Hour
	// MinIPv4Bits and MinIPv6Bits refuse prefixes broad enough to cut off
	// a whole provider or the edge's own networks by mistake.
	MinIPv4Bits = 16
	MinIPv6Bits = 32
	// MaxReasonBytes bounds the operator's note (audit and list only; it
	// never reaches an edge).
	MaxReasonBytes = 256
	// MaxActorBytes bounds the recorded principal.
	MaxActorBytes = 256
	// MaxDocumentBytes bounds the encoded document before decoding.
	MaxDocumentBytes = 256 << 10
	// FormatVersion is the document version.
	FormatVersion = 1
	// IDBytes is the random part of an ID; IDs are lowercase hex.
	IDBytes = 8
)

// Replicated owner framing (SQLite runtime section) and the Kubernetes
// runtime ConfigMap key.
const (
	Section      = "runtime"
	Prefix       = "edge-blocks/"
	Key          = Prefix + "v1/state"
	ConfigMapKey = "edge-blocks.json"
)

// Errors. ErrInvalid and ErrBounds are caller errors (400), ErrExists and
// ErrNotFound map to 409 and 404, ErrState is refused stored evidence.
var (
	ErrInvalid  = errors.New("edgeblock: invalid block")
	ErrBounds   = errors.New("edgeblock: block set is full")
	ErrExists   = errors.New("edgeblock: prefix is already blocked")
	ErrNotFound = errors.New("edgeblock: no such active block")
	ErrState    = errors.New("edgeblock: invalid stored block set")
)

// Block is one stored edge block.
type Block struct {
	ID        string    `json:"id"`
	Prefix    string    `json:"prefix"`
	CreatedAt time.Time `json:"createdAt"`
	Until     time.Time `json:"until"`
	CreatedBy string    `json:"createdBy,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// State is the whole stored set. Revision increases by one with every
// change, so an edge reading several backends can prefer the newest copy of
// the same owner over a lagging replica.
type State struct {
	FormatVersion int     `json:"formatVersion"`
	Revision      uint64  `json:"revision,string"`
	Blocks        []Block `json:"blocks"`
}

// Request is one create command. Exactly one of Duration and Until is set.
type Request struct {
	Prefix   string
	Duration time.Duration
	Until    time.Time
	Reason   string
	Actor    string
}

// ParsePrefix returns the canonical masked prefix of s. A bare address is a
// host prefix (/32 or /128). IPv4-mapped IPv6 is refused: dnsdist matches
// IPv4 clients as IPv4.
func ParsePrefix(s string) (netip.Prefix, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 64 {
		return netip.Prefix{}, fmt.Errorf("%w: prefix is empty or too long", ErrInvalid)
	}
	var p netip.Prefix
	if strings.Contains(s, "/") {
		parsed, err := netip.ParsePrefix(s)
		if err != nil {
			return netip.Prefix{}, fmt.Errorf("%w: %q is not a CIDR prefix", ErrInvalid, s)
		}
		p = parsed
	} else {
		a, err := netip.ParseAddr(s)
		if err != nil || a.Zone() != "" {
			return netip.Prefix{}, fmt.Errorf("%w: %q is not an unzoned address or CIDR prefix", ErrInvalid, s)
		}
		p = netip.PrefixFrom(a, a.BitLen())
	}
	a := p.Addr()
	if a.Zone() != "" || a.Is4In6() {
		return netip.Prefix{}, fmt.Errorf("%w: zoned and IPv4-mapped addresses are not supported", ErrInvalid)
	}
	if a.Is4() && p.Bits() < MinIPv4Bits || a.Is6() && p.Bits() < MinIPv6Bits {
		return netip.Prefix{}, fmt.Errorf("%w: prefix %s is broader than /%d (IPv4) or /%d (IPv6)", ErrInvalid, p, MinIPv4Bits, MinIPv6Bits)
	}
	return p.Masked(), nil
}

// NewID returns a fresh random block ID.
func NewID() (string, error) {
	var b [IDBytes]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// ValidID reports whether id has the shape NewID returns.
func ValidID(id string) bool {
	if len(id) != 2*IDBytes {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// validBlock checks one stored block. Times are whole UTC seconds.
func validBlock(b Block) error {
	if !ValidID(b.ID) {
		return ErrState
	}
	p, err := ParsePrefix(b.Prefix)
	if err != nil || p.String() != b.Prefix {
		return ErrState
	}
	if b.CreatedAt.IsZero() || b.Until.IsZero() || !wholeUTC(b.CreatedAt) || !wholeUTC(b.Until) {
		return ErrState
	}
	if d := b.Until.Sub(b.CreatedAt); d < MinDuration || d > MaxDuration {
		return ErrState
	}
	if !validText(b.CreatedBy, MaxActorBytes) || !validText(b.Reason, MaxReasonBytes) {
		return ErrState
	}
	return nil
}

func wholeUTC(t time.Time) bool {
	return t.Location() == time.UTC && t.Nanosecond() == 0
}

// Validate checks a whole state: version, bounds, unique IDs and unique
// prefixes, sorted by ID.
func (s State) Validate() error {
	if s.FormatVersion != FormatVersion || len(s.Blocks) > MaxBlocks {
		return ErrState
	}
	ids, prefixes := map[string]bool{}, map[string]bool{}
	for i, b := range s.Blocks {
		if err := validBlock(b); err != nil {
			return err
		}
		if ids[b.ID] || prefixes[b.Prefix] || i > 0 && s.Blocks[i-1].ID >= b.ID {
			return ErrState
		}
		ids[b.ID], prefixes[b.Prefix] = true, true
	}
	return nil
}

// Active returns the blocks whose expiry is after now, sorted by ID.
func (s State) Active(now time.Time) []Block {
	out := make([]Block, 0, len(s.Blocks))
	for _, b := range s.Blocks {
		if b.Until.After(now) {
			out = append(out, b)
		}
	}
	return out
}

// Empty is the state of an owner that has never been written.
func Empty() State { return State{FormatVersion: FormatVersion, Blocks: []Block{}} }

// Encode returns the canonical document of a valid state.
func Encode(s State) ([]byte, error) {
	if s.Blocks == nil {
		s.Blocks = []Block{}
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	if len(data) > MaxDocumentBytes {
		return nil, ErrState
	}
	return data, nil
}

// Decode parses and validates a stored document. Unknown fields refuse.
func Decode(data []byte) (State, error) {
	if len(data) == 0 || len(data) > MaxDocumentBytes {
		return State{}, ErrState
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var s State
	if err := dec.Decode(&s); err != nil || dec.More() {
		return State{}, ErrState
	}
	if s.Blocks == nil {
		s.Blocks = []Block{}
	}
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}

// Add returns the state with one new block. Expired blocks are pruned first;
// an active block on the same prefix refuses (revoke it first), and so does
// a full set. now must be a whole UTC second; the expiry rounds up to one.
func Add(s State, req Request, id string, now time.Time) (State, Block, error) {
	if err := s.Validate(); err != nil {
		return State{}, Block{}, err
	}
	if !ValidID(id) || now.IsZero() {
		return State{}, Block{}, ErrInvalid
	}
	now = now.UTC().Truncate(time.Second)
	p, err := ParsePrefix(req.Prefix)
	if err != nil {
		return State{}, Block{}, err
	}
	var until time.Time
	switch {
	case req.Duration != 0 && !req.Until.IsZero():
		return State{}, Block{}, fmt.Errorf("%w: give a duration or an expiry, not both", ErrInvalid)
	case req.Duration != 0:
		until = now.Add(req.Duration)
	case !req.Until.IsZero():
		until = req.Until.UTC()
	default:
		return State{}, Block{}, fmt.Errorf("%w: a duration or an expiry is required", ErrInvalid)
	}
	if until.Nanosecond() != 0 {
		until = until.Truncate(time.Second).Add(time.Second)
	}
	if d := until.Sub(now); d < MinDuration || d > MaxDuration {
		return State{}, Block{}, fmt.Errorf("%w: a block lasts from %s to %s", ErrInvalid, MinDuration, MaxDuration)
	}
	if !validText(req.Reason, MaxReasonBytes) {
		return State{}, Block{}, fmt.Errorf("%w: the reason has control characters or exceeds %d bytes", ErrInvalid, MaxReasonBytes)
	}
	actor := req.Actor
	if len(actor) > MaxActorBytes || !validText(actor, MaxActorBytes) {
		actor = ""
	}
	next := State{FormatVersion: FormatVersion, Revision: s.Revision + 1, Blocks: s.Active(now)}
	for _, b := range next.Blocks {
		if b.Prefix == p.String() {
			return State{}, Block{}, fmt.Errorf("%w: %s is blocked until %s by block %s", ErrExists, b.Prefix, b.Until.Format(time.RFC3339), b.ID)
		}
		if b.ID == id {
			return State{}, Block{}, ErrInvalid
		}
	}
	if len(next.Blocks) >= MaxBlocks {
		return State{}, Block{}, fmt.Errorf("%w: at most %d active blocks", ErrBounds, MaxBlocks)
	}
	block := Block{ID: id, Prefix: p.String(), CreatedAt: now, Until: until, CreatedBy: actor, Reason: req.Reason}
	next.Blocks = append(next.Blocks, block)
	slices.SortFunc(next.Blocks, func(a, b Block) int { return strings.Compare(a.ID, b.ID) })
	if err := next.Validate(); err != nil {
		return State{}, Block{}, err
	}
	return next, block, nil
}

// Revoke returns the state without the active block id. Expired blocks are
// pruned too; revoking an expired or unknown block is ErrNotFound.
func Revoke(s State, id string, now time.Time) (State, Block, error) {
	if err := s.Validate(); err != nil {
		return State{}, Block{}, err
	}
	if !ValidID(id) {
		return State{}, Block{}, ErrNotFound
	}
	active := s.Active(now)
	i := slices.IndexFunc(active, func(b Block) bool { return b.ID == id })
	if i < 0 {
		return State{}, Block{}, ErrNotFound
	}
	removed := active[i]
	next := State{FormatVersion: FormatVersion, Revision: s.Revision + 1, Blocks: slices.Delete(active, i, i+1)}
	return next, removed, nil
}
