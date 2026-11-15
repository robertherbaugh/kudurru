package trustanchors

import (
	"math"
	"slices"
	"strings"
	"time"

	"github.com/miekg/dns"
)

// RecoveryFormatVersion is explicit so an old decoder cannot mistake operator
// trust for RFC hold-down progress. Ordinary format-one states stay unchanged.
const RecoveryFormatVersion = 2

// MaxRecoveries bounds permanent out-of-band trust history independently of keys.
const MaxRecoveries = 32

// Recovery records the explicit replacement trust and exact original pure base.
// It is persisted provenance, never an operator authorization or DNS proof.
type Recovery struct {
	Generation     uint64    `json:"generation,string"`
	PreviousDigest string    `json:"previousDigest"`
	At             time.Time `json:"at"`
	Seeds          []string  `json:"seeds"`
}

// RecoveryObservation can only be issued by Recover after authenticating the
// operator-selected keys. It cannot be used by ordinary Advance or Bootstrap.
// A future operator adapter must capture its real source/writer before input,
// authorize and audit the recovery, and preserve this original proof to commit.
type RecoveryObservation struct {
	base, proposed Version
	proof          Observation
}

// ValidUntil returns the original evidence expiry without renewing it.
func (o RecoveryObservation) ValidUntil() time.Time { return o.proof.ValidUntil() }

// CheckLifetime retains both the original wall and monotonic receipt bounds.
func (o RecoveryObservation) CheckLifetime(at Sample) error { return o.proof.CheckLifetime(at) }

// CheckRecovery binds the complete before/after histories and original proof.
// Encoding, decoding or copying equal diagnostic fields cannot issue this proof.
func (o RecoveryObservation) CheckRecovery(before, proposed State, at Sample) error {
	if o.base.Generation == 0 || o.proposed.Generation != o.base.Generation+1 || !o.proof.authenticated {
		return ErrProof
	}
	base, err := before.Version()
	if err != nil {
		return err
	}
	next, err := proposed.Version()
	if err != nil {
		return err
	}
	if base != o.base || next != o.proposed {
		return ErrStale
	}
	return o.CheckLifetime(at)
}

// Recover proposes explicit out-of-band recovery ONLY after all anchors are
// lost. It never changes initial policy/seed provenance, revives a revoked key,
// removes a tombstone, invents elapsed hold-down or accepts this proposal itself.
// This is not an automatic retry or a replacement for operator authorization.
func Recover(before State, explicitDS []string, message *dns.Msg, at Sample) (State, RecoveryObservation, error) {
	base, err := before.Version()
	if err != nil {
		return State{}, RecoveryObservation{}, err
	}
	old := before.Inspect()
	if !old.Quarantined {
		return State{}, RecoveryObservation{}, ErrInvalid
	}
	if base.Generation >= math.MaxUint64-2 || len(old.Recoveries) >= MaxRecoveries {
		return State{}, RecoveryObservation{}, ErrBounds
	}
	if !at.valid() || at.wall.Before(old.Checkpoint.Wall) || at.session == old.Checkpoint.Session && at.tick < old.Checkpoint.Tick {
		return State{}, RecoveryObservation{}, ErrClock
	}
	fresh, proof, err := Bootstrap(Seed{ClusterID: old.ClusterID, PolicyID: old.PolicyID, DS: explicitDS}, message, at)
	if err != nil {
		return State{}, RecoveryObservation{}, err
	}
	selected := make([]*dns.DS, 0, len(fresh.doc.Seeds))
	for _, text := range fresh.doc.Seeds {
		ds, err := parseSeed(text)
		if err != nil {
			return State{}, RecoveryObservation{}, err
		}
		selected = append(selected, ds)
	}
	denied := make(map[string]Key)
	for _, key := range old.Keys {
		if key.Status == Revoked {
			rr, err := dns.NewRR(key.DNSKEY)
			if err != nil {
				return State{}, RecoveryObservation{}, ErrInvalid
			}
			if seeded(rr.(*dns.DNSKEY), selected) {
				return State{}, RecoveryObservation{}, ErrProof
			}
			denied[key.ID] = key
		}
	}
	next := fresh.Inspect()
	next.Keys = slices.DeleteFunc(next.Keys, func(k Key) bool { _, found := denied[k.ID]; return found })
	for _, key := range denied {
		next.Keys = append(next.Keys, key)
	}
	slices.SortFunc(next.Keys, func(a, b Key) int { return strings.Compare(a.ID, b.ID) })
	next.Format, next.Generation = RecoveryFormatVersion, base.Generation+1
	next.Seeds, next.SeedDigest = old.Seeds, old.SeedDigest
	next.Recoveries = append(old.Recoveries, Recovery{Generation: next.Generation, PreviousDigest: base.Digest, At: at.wall, Seeds: slices.Clone(fresh.doc.Seeds)})
	proposed := State{doc: next}
	v, err := proposed.Version()
	if err != nil {
		return State{}, RecoveryObservation{}, err
	}
	return proposed, RecoveryObservation{base: base, proposed: v, proof: proof}, nil
}

// RetainsRecoveryHistory checks structural provenance only. An installer may
// skip intermediate generations, but cannot rewrite known recovery records or
// insert retroactive trust into a generation the receiver already accepted.
// This comparison grants neither operator authority nor permission to install.
func (s State) RetainsRecoveryHistory(before State) bool {
	old, next := before.doc.Recoveries, s.doc.Recoveries
	if len(next) < len(old) || before.doc.Format > s.doc.Format {
		return false
	}
	for i, r := range old {
		v := next[i]
		if r.Generation != v.Generation || r.PreviousDigest != v.PreviousDigest || !r.At.Equal(v.At) || !slices.Equal(r.Seeds, v.Seeds) {
			return false
		}
	}
	for _, r := range next[len(old):] {
		if r.Generation <= before.doc.Generation {
			return false
		}
	}
	return true
}

// Recovered trust is explicit and permanently bounded. Every listed DS must
// match an actually accepted key at that operation's time; unused seeds cannot
// silently authorize a future key. Initial seed provenance remains separate.
func validateRecoveries(d Snapshot) ([]*dns.DS, error) {
	if d.Format == FormatVersion {
		if len(d.Recoveries) != 0 {
			return nil, ErrInvalid
		}
		return nil, nil
	}
	if len(d.Recoveries) == 0 || len(d.Recoveries) > MaxRecoveries {
		return nil, ErrBounds
	}
	var seeds []*dns.DS
	seen := make(map[string]bool)
	for i, recovery := range d.Recoveries {
		if recovery.Generation < 2 || recovery.Generation > d.Generation || !validDigest(recovery.PreviousDigest) || !validTime(recovery.At) || recovery.At.After(d.Checkpoint.Wall) ||
			len(recovery.Seeds) == 0 || len(recovery.Seeds) > MaxLiveKeys || !slices.IsSorted(recovery.Seeds) ||
			i > 0 && (recovery.Generation <= d.Recoveries[i-1].Generation || recovery.At.Before(d.Recoveries[i-1].At)) {
			return nil, ErrInvalid
		}
		for _, text := range recovery.Seeds {
			if seen[text] || len(seeds) >= MaxLiveKeys+MaxTombstones {
				return nil, ErrBounds
			}
			ds, err := parseSeed(text)
			if err != nil {
				return nil, err
			}
			matched := false
			for _, key := range d.Keys {
				if !key.AcceptedAt.Equal(recovery.At) || key.Status == AddPending {
					continue
				}
				rr, err := dns.NewRR(key.DNSKEY)
				if err != nil {
					return nil, ErrInvalid
				}
				if k, ok := rr.(*dns.DNSKEY); ok && seeded(k, []*dns.DS{ds}) {
					matched = true
				}
			}
			if !matched {
				return nil, ErrInvalid
			}
			seen[text] = true
			seeds = append(seeds, ds)
		}
	}
	return seeds, nil
}
