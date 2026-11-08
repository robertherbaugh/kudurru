// Package accepted frames the complete enrollment owner for durable storage.
// Framing and history checks grant neither operator nor member authorization.
package accepted

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/google/uuid"
	"github.com/robertherbaugh/kudurru/internal/cluster/enrollment"
	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// One complete row has a separate namespace and a bounded base64 state payload.
const (
	Section           = "membership"
	Prefix            = "enrollment/"
	RootKey           = Prefix + "root"
	MaxRowBytes       = 768 << 10
	AwaitingOwnerJSON = `{"phase":"awaiting","version":1}`
)

// ErrOwner reports absent, malformed, foreign or rewritten complete history.
var ErrOwner = errors.New("enrollment/accepted: invalid complete owner")

type document struct {
	Version int    `json:"version"`
	Cluster string `json:"cluster"`
	// Bytes retain the pure codec exactly through shared JSON canonicalization.
	State []byte `json:"state"`
}

// Owner is immutable, including its explicit never-enrolled dormant state.
// A zero Owner is invalid. Constructors describe proposals, not durable effects.
type Owner struct {
	cluster string
	state   *enrollment.State
	row     replication.Entry
	digest  string
}

// Dormant proposes an explicit never-enrolled owner for migration initialization.
func Dormant(cluster string) (Owner, error) { return makeOwner(cluster, nil) }

// Enrolled frames an already checked pure successor. Persistence must separately
// verify the original transition, authorization, source CAS and final admission.
func Enrolled(state *enrollment.State) (Owner, error) {
	if state == nil {
		return Owner{}, ErrOwner
	}
	return makeOwner(state.ClusterID(), state)
}

func makeOwner(cluster string, state *enrollment.State) (Owner, error) {
	id, err := uuid.Parse(cluster)
	if err != nil || id == uuid.Nil || id.String() != cluster {
		return Owner{}, ErrOwner
	}
	d := document{Version: 1, Cluster: cluster}
	if state != nil {
		d.State, err = state.Encode()
		if err != nil {
			return Owner{}, err
		}
	}
	raw, err := json.Marshal(d)
	if err != nil {
		return Owner{}, ErrOwner
	}
	row, err := canonical(replication.Entry{Key: RootKey, Data: raw})
	if err != nil {
		return Owner{}, err
	}
	sum := sha256.Sum256(row.Data)
	return Owner{cluster: cluster, state: state, row: row, digest: hex.EncodeToString(sum[:])}, nil
}

// Digest identifies the entire bound owner, including explicit dormancy.
func (o Owner) Digest() string { return o.digest }

// ClusterID is the actual owner cluster, never a supplied target address.
func (o Owner) ClusterID() string { return o.cluster }

// State returns immutable pure history; nil means dormant only for a valid Owner.
func (o Owner) State() *enrollment.State { return o.state }

// Rows returns detached canonical bytes for the exact complete owner.
func (o Owner) Rows() ([]replication.Entry, error) {
	if o.digest == "" {
		return nil, ErrOwner
	}
	r := o.row
	r.Data = bytes.Clone(r.Data)
	return []replication.Entry{r}, nil
}

func canonical(row replication.Entry) (replication.Entry, error) {
	if row.Key != RootKey || len(row.Data) > MaxRowBytes {
		return replication.Entry{}, ErrOwner
	}
	return replication.ValidateEntry(Section, row, replication.Limits{MaxObjectBytes: MaxRowBytes, MaxDepth: 4})
}

func owned(rows []replication.Entry) (replication.Entry, error) {
	var out replication.Entry
	for _, row := range rows {
		if !strings.HasPrefix(row.Key, Prefix) {
			continue
		}
		if out.Key != "" || row.Key != RootKey {
			return replication.Entry{}, ErrOwner
		}
		out = row
	}
	if out.Key == "" {
		return replication.Entry{}, ErrOwner
	}
	return canonical(out)
}

// IsAwaiting accepts only the physical migration's exact non-granting sentinel.
// It does not make that sentinel a normal complete image or enrolled history.
func IsAwaiting(rows []replication.Entry) bool {
	r, err := owned(rows)
	return err == nil && bytes.Equal(r.Data, []byte(AwaitingOwnerJSON))
}

// DecodeRows requires a complete normal owner and preserves pure canonical bytes.
// Unknown versions, additional rows, absent history and foreign clusters refuse.
func DecodeRows(rows []replication.Entry, cluster string) (Owner, error) {
	r, err := owned(rows)
	if err != nil {
		return Owner{}, err
	}
	var d document
	decoder := json.NewDecoder(bytes.NewReader(r.Data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&d) != nil || d.Version != 1 || d.Cluster != cluster {
		return Owner{}, ErrOwner
	}
	var state *enrollment.State
	if d.State != nil {
		state, err = enrollment.Decode(d.State)
		if err != nil || state.ClusterID() != cluster {
			return Owner{}, ErrOwner
		}
	}
	o, err := makeOwner(cluster, state)
	if err != nil || !bytes.Equal(r.Data, o.row.Data) {
		return Owner{}, ErrOwner
	}
	return o, nil
}

// RetainsHistory refuses removal or resurrection of an already enrolled owner.
// Dormant activation still needs explicit writer/operator admission elsewhere.
func (o Owner) RetainsHistory(old Owner) bool {
	if o.digest == "" || old.digest == "" || o.cluster != old.cluster {
		return false
	}
	if old.state == nil {
		return true
	}
	return o.state != nil && o.state.RetainsHistory(old.state)
}

// ValidateEntry validates only normal rows belonging to this owner.
func ValidateEntry(section string, row replication.Entry) error {
	if section != Section || !strings.HasPrefix(row.Key, Prefix) {
		return nil
	}
	r, err := canonical(row)
	if err != nil {
		return err
	}
	var d document
	if json.Unmarshal(r.Data, &d) != nil {
		return ErrOwner
	}
	_, err = DecodeRows([]replication.Entry{r}, d.Cluster)
	return err
}

// ValidateImage requires explicit normal complete history, including dormancy.
func ValidateImage(image *replication.Image) error {
	if image == nil {
		return ErrOwner
	}
	o, err := DecodeRows(image.Payload.Membership, image.Term.ClusterID)
	if err != nil {
		return err
	}
	if o.state != nil && o.state.UpdatedAt().After(image.CreatedAt) {
		return ErrOwner
	}
	return nil
}
