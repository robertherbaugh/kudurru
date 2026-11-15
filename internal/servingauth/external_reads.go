package servingauth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// ExternalAccountInfo is a detached administration projection. It contains no
// session evidence, token hashes or provider credentials.
type ExternalAccountInfo struct {
	Name, Revision string
	Identity       ExternalIdentity
	ValidAfter     time.Time
}

// ExternalAccountRevision binds an operator's review to this exact identity and
// its revocation floor, including a disable/enable cycle with identical profile.
func ExternalAccountRevision(i ExternalIdentity, floor time.Time) (string, error) {
	if !validExternalIdentity(i) {
		return "", ErrInvalid
	}
	b, err := json.Marshal(struct {
		Identity ExternalIdentity
		Floor    time.Time
	}{i, floor.UTC()})
	if err != nil {
		return "", ErrInvalid
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

// ExternalAccounts reads only the captured complete inventory. Its epoch can
// bind pagination; a later page cannot silently mix different security cuts.
func (v *View) ExternalAccounts() ([]ExternalAccountInfo, uint64, error) {
	if v == nil || v.released.Load() {
		return nil, 0, ErrInvalid
	}
	out := make([]ExternalAccountInfo, 0, len(v.externalIdentities))
	for account, identity := range v.externalIdentities {
		floor := v.accounts[account]
		revision, err := ExternalAccountRevision(identity, floor)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, ExternalAccountInfo{Name: account.Name, Revision: revision, Identity: detachedExternal(identity), ValidAfter: floor})
	}
	slices.SortFunc(out, func(a, b ExternalAccountInfo) int { return strings.Compare(a.Name, b.Name) })
	return out, v.cut.security.Epoch, nil
}
