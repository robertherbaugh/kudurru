package servingauth

import (
	"bytes"
	"encoding/json"
	"math"
	"time"

	"github.com/robertherbaugh/kudurru/internal/store/replication"
)

// rootV4Entry freezes the complete pre-checkpoint manifest for explicit startup
// migration. Ordinary reads and replica installs accept only the current schema.
type rootV4Entry struct {
	FormatVersion      int      `json:"formatVersion"`
	Epoch              uint64   `json:"epoch,string"`
	GrantFloors        []string `json:"grantFloors"`
	AccountFloors      []string `json:"accountFloors"`
	ProviderFloors     []string `json:"providerFloors"`
	Sessions           []string `json:"sessions"`
	TOTP               []string `json:"totp"`
	ExternalIdentities []string `json:"externalIdentities"`
	ExternalSessions   []string `json:"externalSessions"`
	BrowserAttempts    []string `json:"browserAttempts"`
}

// UpgradeSecurityV4 preserves every credential, floor and replay record. Only an
// admitted writer can subsequently fold revocations into the checkpoint.
func UpgradeSecurityV4(rows []replication.Entry) (Security, error) {
	s, err := decodeSecurity(rows, 4)
	if err != nil {
		return Security{}, err
	}
	if s.Epoch == math.MaxUint64 {
		return Security{}, ErrBounds
	}
	s.Epoch++
	return s, nil
}

func validCheckpoint(at time.Time) bool { return at.IsZero() || validTime(at) }

func validRootCheckpoint(raw []byte, at time.Time) bool {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return false
	}
	value, ok := fields["revocationCheckpoint"]
	return ok && !bytes.Equal(value, []byte("null")) && validCheckpoint(at)
}

// AfterCheckpoint admits new OIDC proof and new credential enrollment only
// strictly after the compacted revocation history. Existing stored credentials
// are checked by their own enrollment/token and retained exact floors instead.
func AfterCheckpoint(checkpoint, issuedAt time.Time) bool {
	return checkpoint.IsZero() || issuedAt.After(checkpoint)
}
