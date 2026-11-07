// Package trustanchors implements a bounded, immutable RFC 5011 root-anchor
// state machine. It performs no storage, publication, election, or network I/O.
// An observation is evidence for one exact state, never writer admission.
package trustanchors

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"slices"
	"strings"
	"time"
)

// Protocol constants are production values, not accelerated test parameters.
const (
	FormatVersion      = 1
	AddHoldDown        = 30 * 24 * time.Hour
	RemoveHoldDown     = 30 * 24 * time.Hour
	MinRefresh         = time.Hour
	MaxRefresh         = 15 * 24 * time.Hour
	MaxRetry           = 24 * time.Hour
	ClockTolerance     = 5 * time.Minute
	MaxLiveKeys        = 64
	MaxTombstones      = 256
	MaxSignatures      = 64
	MaxSignatureChecks = 128
	MaxWireBytes       = 64 << 10
	MaxStateBytes      = 2 << 20
)

// Errors expose fixed categories without including received records or proofs.
var (
	ErrInvalid = errors.New("trustanchors: invalid state or input")
	ErrBounds  = errors.New("trustanchors: bound exceeded")
	ErrProof   = errors.New("trustanchors: unauthenticated root observation")
	ErrStale   = errors.New("trustanchors: original state changed")
	ErrClock   = errors.New("trustanchors: invalid clock continuity")
	ErrExpired = errors.New("trustanchors: original observation expired")
)

// Status names the persisted RFC 5011 states. Revoked records are permanent
// tombstones; this implementation does not discard them at RemoveHoldDown.
type Status string

// Persisted lifecycle states retain pending, trusted, missing, or revoked history.
const (
	AddPending Status = "AddPend"
	Valid      Status = "Valid"
	Missing    Status = "Missing"
	Revoked    Status = "Revoked"
)

// SessionClock supplies actual wall time and process-local monotonic progress.
// A new clock always has a new session; persisted IDs cannot resume its clock.
type SessionClock struct {
	id    string
	start time.Time
}

// Sample is opaque so production callers cannot supply claimed elapsed time.
// The future runtime adapter still owns original context/deadline admission.
type Sample struct {
	session string
	tick    time.Duration
	wall    time.Time
}

// NewSessionClock starts an independent process-local accounting session.
func NewSessionClock() (*SessionClock, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	return &SessionClock{id: hex.EncodeToString(id[:]), start: time.Now()}, nil
}

// Sample captures actual wall and monotonic time without extending a deadline.
func (c *SessionClock) Sample() Sample {
	if c == nil || c.id == "" || c.start.IsZero() {
		return Sample{}
	}
	at := time.Now()
	return Sample{session: c.id, tick: at.Sub(c.start), wall: at.UTC()}
}

// Checkpoint is the last durably accepted session sample. Only progress after
// this checkpoint in the SAME session can contribute to candidate hold-down.
// A checkpoint in a proposed Bootstrap/Advance result is only a claim until the
// caller's exact original-admission durable CAS succeeds. Do not chain proposed,
// refused, rolled-back or merely encoded/decoded states as accepted input.
type Checkpoint struct {
	Session string        `json:"session"`
	Tick    time.Duration `json:"tick,string"`
	Wall    time.Time     `json:"wall"`
}

// Key retains exact key material and the original acceptance/revocation history.
// ID clears only REVOKE for identity; DNSKEY retains the actual flags.
type Key struct {
	ID           string        `json:"id"`
	DNSKEY       string        `json:"dnskey"`
	Status       Status        `json:"status"`
	FirstSeen    time.Time     `json:"firstSeen"`
	LastSeen     time.Time     `json:"lastSeen"`
	AcceptedAt   time.Time     `json:"acceptedAt"`
	OriginalTTL  uint32        `json:"originalTTL"`
	Elapsed      time.Duration `json:"elapsed,string"`
	Support      []string      `json:"support"`
	MissingSince time.Time     `json:"missingSince"`
	RevokedAt    time.Time     `json:"revokedAt"`
}

// Schedule retains the original authenticated scheduler basis. Reading or
// restoring it never creates a new observation or restamps a due instant.
type Schedule struct {
	ObservedAt     time.Time     `json:"observedAt"`
	OriginalTTL    uint32        `json:"originalTTL"`
	SignatureUntil time.Time     `json:"signatureUntil"`
	NextRefresh    time.Time     `json:"nextRefresh"`
	RetryInterval  time.Duration `json:"retryInterval,string"`
}

// Snapshot is a detached diagnostic/persistence document, not a mutable owner.
// Quarantined is explicit: an empty live set never means shipped defaults.
type Snapshot struct {
	Format         int        `json:"format"`
	ClusterID      string     `json:"clusterID"`
	PolicyID       string     `json:"policyID"`
	SeedDigest     string     `json:"seedDigest"`
	Seeds          []string   `json:"seeds"`
	Generation     uint64     `json:"generation,string"`
	Keys           []Key      `json:"keys"`
	Checkpoint     Checkpoint `json:"checkpoint"`
	Schedule       Schedule   `json:"schedule"`
	ClockUncertain bool       `json:"clockUncertain"`
	Quarantined    bool       `json:"quarantined"`
}

// State holds immutable protocol evidence. Bootstrap/Advance produce PROPOSED
// states; this pure package cannot certify persistence. The caller must durably
// CAS each result against its exact original base before supplying that result
// as the next accepted input. Decode is not proof of an original authorized CAS.
type State struct{ doc Snapshot }

// Version is the exact canonical owner version required by a future same-tx CAS.
type Version struct {
	Generation uint64
	Digest     string
}

// Inspect returns detached fields; callers cannot alter this state's authority.
func (s State) Inspect() Snapshot {
	d := s.doc
	d.Seeds = slices.Clone(d.Seeds)
	d.Keys = slices.Clone(d.Keys)
	for i := range d.Keys {
		d.Keys[i].Support = slices.Clone(d.Keys[i].Support)
	}
	return d
}

// Version binds all persisted history, not merely the effective anchor bytes.
func (s State) Version() (Version, error) {
	b, err := Encode(s)
	if err != nil {
		return Version{}, err
	}
	return Version{Generation: s.doc.Generation, Digest: digest(b)}, nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func validDigest(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == sha256.Size && hex.EncodeToString(b) == s
}

func validSession(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 16 && hex.EncodeToString(b) == s
}

func validTime(t time.Time) bool {
	return !t.IsZero() && t.Location() == time.UTC && t.Unix() > 0 && t.Unix() < math.MaxUint32
}

func (s Sample) valid() bool {
	return validSession(s.session) && s.tick >= 0 && validTime(s.wall)
}

func trusted(k Key) bool { return k.Status == Valid || k.Status == Missing }

func holdDown(ttl uint32) time.Duration {
	return max(AddHoldDown, time.Duration(ttl)*time.Second)
}

// RefreshIntervals implements RFC 5011 section 2.3 from one original proof.
func RefreshIntervals(ttl uint32, observed, signatureUntil time.Time) (time.Duration, time.Duration, error) {
	if !validTime(observed) || !validTime(signatureUntil) || !signatureUntil.After(observed) {
		return 0, 0, ErrInvalid
	}
	lifetime := signatureUntil.Sub(observed)
	original := time.Duration(ttl) * time.Second
	return max(MinRefresh, min(MaxRefresh, original/2, lifetime/2)), max(MinRefresh, min(MaxRetry, original/10, lifetime/10)), nil
}

func schedule(at time.Time, ttl uint32, until time.Time) (Schedule, error) {
	refresh, retry, err := RefreshIntervals(ttl, at, until)
	if err != nil {
		return Schedule{}, err
	}
	return Schedule{ObservedAt: at, OriginalTTL: ttl, SignatureUntil: until, NextRefresh: at.Add(refresh), RetryInterval: retry}, nil
}

// Advance returns a detached next state from exact cryptographic evidence.
// It does NOT commit, publish, capture a writer or retry a refused operation.
// Its new Checkpoint is not accepted progress until the caller successfully
// commits the exact original-admission CAS; never chain an uncommitted result.
func Advance(s State, o Observation, final Sample) (State, error) {
	v, err := s.Version()
	if err != nil {
		return State{}, err
	}
	if !o.verified || v != o.base {
		return State{}, ErrStale
	}
	if err := o.CheckLifetime(final); err != nil {
		return State{}, err
	}
	if final.wall.Before(s.doc.Checkpoint.Wall) || o.at.wall.Before(s.doc.Checkpoint.Wall) {
		return State{}, ErrClock
	}
	if s.doc.Generation == math.MaxUint64 {
		return State{}, ErrBounds
	}
	next := s.Inspect()
	previous := next.Checkpoint
	credit := time.Duration(0)
	uncertain := o.at.wall.Before(previous.Wall)
	if o.at.session == previous.Session {
		if o.at.tick < previous.Tick {
			return State{}, ErrClock
		}
		credit = o.at.tick - previous.Tick
		wall := o.at.wall.Sub(previous.Wall)
		if wall < 0 || wall-credit > ClockTolerance || credit-wall > ClockTolerance {
			uncertain = true
		}
	}
	if uncertain {
		credit = 0
	}
	next.ClockUncertain = uncertain
	next.Checkpoint = Checkpoint{Session: final.session, Tick: final.tick, Wall: final.wall}
	next.Generation++
	// Revocation precedes candidate processing. It never waits for add time or
	// disappears merely because its owner is no longer an ordinary authenticator.
	for i := range next.Keys {
		k := &next.Keys[i]
		if rr, ok := o.revoked[k.ID]; ok && k.Status != Revoked {
			k.Status, k.DNSKEY, k.RevokedAt = Revoked, rr, o.at.wall
			k.LastSeen, k.MissingSince = o.at.wall, time.Time{}
		}
	}
	reset := map[string]bool{}
	for _, k := range next.Keys {
		if k.Status != AddPending {
			continue
		}
		alive := false
		for _, id := range k.Support {
			for _, supporter := range next.Keys {
				if supporter.ID == id && trusted(supporter) {
					alive = true
				}
			}
		}
		if !alive {
			reset[k.ID] = true
		}
	}
	if len(reset) > 0 {
		kept := make([]Key, 0, len(next.Keys))
		for _, k := range next.Keys {
			if !reset[k.ID] {
				kept = append(kept, k)
			}
		}
		next.Keys = kept
	}
	if o.authenticated {
		kept := make([]Key, 0, len(next.Keys)+len(o.keys))
		known := make(map[string]bool, len(next.Keys))
		for id := range reset {
			known[id] = true
		}
		for _, k := range next.Keys {
			known[k.ID] = true
			_, present := o.keys[k.ID]
			if k.Status == Revoked {
				kept = append(kept, k)
				continue
			}
			if k.Status == AddPending {
				alive := false
				for _, id := range k.Support {
					for _, old := range next.Keys {
						if old.ID == id && trusted(old) {
							alive = true
						}
					}
				}
				// Missing or wholly revoked original support resets acceptance;
				// do not start it again under another supporter in this same cut.
				if !present || !alive {
					continue
				}
				k.Elapsed = min(holdDown(k.OriginalTTL), k.Elapsed+min(credit, holdDown(k.OriginalTTL)))
				if !uncertain && k.Elapsed >= holdDown(k.OriginalTTL) && o.at.wall.After(k.FirstSeen.Add(holdDown(k.OriginalTTL))) {
					k.Status = Valid
					k.AcceptedAt = o.at.wall
				}
			} else if present {
				k.Status, k.MissingSince = Valid, time.Time{}
			} else if k.Status == Valid {
				k.Status, k.MissingSince = Missing, o.at.wall
			}
			if present {
				k.LastSeen = o.at.wall
			}
			kept = append(kept, k)
		}
		for id, rr := range o.keys {
			if known[id] {
				continue
			}
			kept = append(kept, Key{ID: id, DNSKEY: rr, Status: AddPending, FirstSeen: o.at.wall, LastSeen: o.at.wall, OriginalTTL: o.holdTTL, Support: slices.Clone(o.support)})
		}
		next.Keys = kept
		next.Schedule, err = schedule(o.at.wall, o.refreshTTL, o.until)
		if err != nil {
			return State{}, err
		}
	}
	live := 0
	for _, k := range next.Keys {
		if trusted(k) {
			live++
		}
	}
	next.Quarantined = live == 0
	slices.SortFunc(next.Keys, func(a, b Key) int { return strings.Compare(a.ID, b.ID) })
	out := State{doc: next}
	if _, err := Encode(out); err != nil {
		return State{}, err
	}
	return out, nil
}
