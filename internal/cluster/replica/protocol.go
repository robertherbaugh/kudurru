// Package replica implements the authenticated, versioned peer transport and
// manual promotion coordinator. It is not a consensus or membership authority.
package replica

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// Routes, metadata headers and default bounds define replication protocol v1.
const (
	PathImage                    = "/internal/replication/v1/image"
	PathWrite                    = "/internal/replication/v1/write"
	HeaderMember                 = "X-Kudurru-Replica-Member"
	HeaderCluster                = "X-Kudurru-Replica-Cluster"
	HeaderTerm                   = "X-Kudurru-Replica-Term"
	DefaultMaxCommandBytes int64 = 2 << 20
	DefaultMaxResultBytes  int64 = 2 << 20
	DefaultCommandLifetime       = 10 * time.Minute
)

// Protocol errors carry no peer payload or credential material.
var (
	ErrUnauthorized   = errors.New("replica: unauthorized peer")
	ErrForbidden      = errors.New("replica: principal forbidden")
	ErrUnavailable    = errors.New("replica: primary unavailable")
	ErrOutcomeUnknown = errors.New("replica: write outcome unknown; retry the same command ID")
	ErrProtocol       = errors.New("replica: invalid protocol")
	ErrLoop           = errors.New("replica: forwarding loop")
	ErrRedirect       = errors.New("replica: redirect rejected")
	ErrUntrusted      = errors.New("replica: untrusted TLS peer")
	ErrDedupConflict  = errors.New("replica: command ID reused with different content")
	ErrExpired        = errors.New("replica: command expired")
	ErrSensitive      = errors.New("replica: sensitive result requires owner sealing")
	ErrIneligible     = errors.New("replica: promotion ineligible")
)

// Peer comes from enrollment, not discovery or an arbitrary public URL.
type Peer struct{ MemberID, ClusterID, Address string }

// PeerResolver returns an enrolled member with its pinned internal address.
type PeerResolver func(context.Context) (Peer, error)

// PeerAuthorizer checks enrollment and any stronger request-bound peer identity.
type PeerAuthorizer func(context.Context, Peer, *http.Request) error

// AuthorityObserver persists authenticated terms before any body consumption.
type AuthorityObserver interface {
	ObserveAuthority(context.Context, replication.Term) (replication.Authority, error)
}

// ExchangeError distinguishes authenticated reachability from accepted images.
// Incompatible data must not manufacture network-loss promotion eligibility.
type ExchangeError struct {
	Reached bool
	Err     error
}

func (e *ExchangeError) Error() string { return e.Err.Error() }
func (e *ExchangeError) Unwrap() error { return e.Err }

func encodeTerm(t replication.Term) string {
	b, _ := json.Marshal(t)
	return base64.RawURLEncoding.EncodeToString(b)
}
func decodeTerm(h string) (replication.Term, error) {
	var t replication.Term
	if len(h) == 0 || len(h) > 4096 {
		return t, ErrProtocol
	}
	b, err := base64.RawURLEncoding.DecodeString(h)
	if err != nil {
		return t, ErrProtocol
	}
	if err := decodeStrict(b, &t); err != nil {
		return t, err
	}
	var v map[string]json.RawMessage
	if err := json.Unmarshal(b, &v); err != nil {
		return t, ErrProtocol
	}
	var epoch string
	if err := json.Unmarshal(v["epoch"], &epoch); err != nil {
		return t, ErrProtocol
	}
	n, err := strconv.ParseUint(epoch, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != epoch {
		return t, ErrProtocol
	}
	if err := t.Validate(); err != nil {
		return t, err
	}
	return t, nil
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 || limit == int64(^uint64(0)>>1) {
		return nil, ErrProtocol
	}
	b, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, replication.ErrTooLarge
	}
	return b, nil
}

// canonicalJSON checks duplicate keys/depth/trailing input before hashing or
// decoding. Number spellings are preserved; float64 conversions are never used.
func canonicalJSON(b []byte) ([]byte, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	v, err := jsonValue(d, 64)
	if err != nil {
		return nil, err
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, ErrProtocol
	}
	return json.Marshal(v)
}
func jsonValue(d *json.Decoder, depth int) (any, error) {
	if depth <= 0 {
		return nil, replication.ErrTooLarge
	}
	t, err := d.Token()
	if err != nil {
		return nil, ErrProtocol
	}
	if x, ok := t.(json.Delim); ok {
		switch x {
		case '{':
			m := map[string]any{}
			for d.More() {
				k, err := d.Token()
				if err != nil {
					return nil, ErrProtocol
				}
				key, ok := k.(string)
				if !ok {
					return nil, ErrProtocol
				}
				if _, ok := m[key]; ok {
					return nil, ErrProtocol
				}
				v, err := jsonValue(d, depth-1)
				if err != nil {
					return nil, err
				}
				m[key] = v
			}
			if _, err := d.Token(); err != nil {
				return nil, ErrProtocol
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
			if _, err := d.Token(); err != nil {
				return nil, ErrProtocol
			}
			return a, nil
		default:
			return nil, ErrProtocol
		}
	}
	return t, nil
}
func decodeStrict(b []byte, out any) error {
	b, err := canonicalJSON(b)
	if err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(out); err != nil {
		return ErrProtocol
	}
	return nil
}
func fingerprint(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func validID(s string) bool {
	if len(s) == 0 || len(s) > 256 {
		return false
	}
	for _, r := range s {
		if r < 33 || r > 126 || strings.ContainsRune("/\\", r) {
			return false
		}
	}
	return true
}

// Problem messages contain no source object, credential or untrusted body.
type Problem struct {
	Code string `json:"code"`
}

func problem(err error) (int, string) {
	switch {
	case errors.Is(err, ErrOutcomeUnknown):
		return 502, "write_outcome_unknown"
	case errors.Is(err, ErrUnauthorized):
		return 401, "unauthorized"
	case errors.Is(err, ErrForbidden):
		return 403, "forbidden"
	case errors.Is(err, ErrExpired):
		return 409, "command_expired"
	case errors.Is(err, ErrDedupConflict):
		return 409, "command_id_conflict"
	case errors.Is(err, replication.ErrCAS):
		return 409, "authority_changed"
	case errors.Is(err, replication.ErrPromotionConflict):
		return 409, "promotion_conflict"
	case errors.Is(err, replication.ErrFenced):
		return 409, "fenced"
	case errors.Is(err, replication.ErrReadOnly):
		return 409, "read_only"
	case errors.Is(err, replication.ErrWrongCluster):
		return 409, "wrong_cluster"
	case errors.Is(err, replication.ErrTooLarge):
		return 413, "too_large"
	case errors.Is(err, ErrLoop):
		return 400, "forwarding_loop"
	case errors.Is(err, ErrProtocol), errors.Is(err, replication.ErrInvalid):
		return 400, "invalid_command"
	case errors.Is(err, ErrUnavailable):
		return 503, "primary_unavailable"
	default:
		return 500, "internal"
	}
}
func problemError(code string) error {
	switch code {
	case "write_outcome_unknown":
		return ErrOutcomeUnknown
	case "unauthorized":
		return ErrUnauthorized
	case "forbidden":
		return ErrForbidden
	case "command_expired":
		return ErrExpired
	case "command_id_conflict":
		return ErrDedupConflict
	case "authority_changed":
		return replication.ErrCAS
	case "promotion_conflict":
		return replication.ErrPromotionConflict
	case "fenced":
		return replication.ErrFenced
	case "read_only":
		return replication.ErrReadOnly
	case "wrong_cluster":
		return replication.ErrWrongCluster
	case "too_large":
		return replication.ErrTooLarge
	case "forwarding_loop":
		return ErrLoop
	case "invalid_command":
		return ErrProtocol
	case "primary_unavailable":
		return ErrUnavailable
	default:
		return fmt.Errorf("%w: peer failure", ErrProtocol)
	}
}
