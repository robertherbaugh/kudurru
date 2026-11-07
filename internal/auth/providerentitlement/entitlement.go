// Package providerentitlement projects captured verified management policy and
// selects existing cluster entitlement receipts. It neither admits authority nor
// resets floors. Root owns those actual commands and the immutable host lifecycle.
package providerentitlement

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/auth/providerregistry"
	"github.com/robertherbaugh/kudurru/internal/license"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
)

// ErrContinuity denies missing, changed or incompatible entitlement evidence.
// Its diagnostic never includes license, provider or credential data.
var ErrContinuity = errors.New("providerentitlement: continuity unavailable")

const entitlementPrefix = "entitlement/v1/"

func hash(value []byte) string {
	d := sha256.Sum256(value)
	return hex.EncodeToString(d[:])
}

func canonicalHex(value string, size int) bool {
	if len(value) != size || strings.ToLower(value) != value {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// Profile projects currently verified management POLICY equivalence, not equality
// of complete signed claims. It requires OIDC entitlement. Active and grace cuts
// share a profile and the original absolute grace deadline. Source, installation,
// signed iat/nbf and signature representation are not profile fields; current-time
// eligibility still requires Cut.Current and the captured policy.
//
// Root holds actual license.WithCurrent through capture/command/publication and
// rechecks the cut at final admission. Installation changes require root reset and
// floors even when this profile matches. Missing or denied cuts grant nothing.
func Profile(cut license.Cut) (string, time.Time, error) {
	if !cut.Current() {
		return "", time.Time{}, ErrContinuity
	}
	s := cut.Status()
	if (s.State != license.StateActive && s.State != license.StateGrace) || !s.Allows(license.FeatureOIDC) || s.ID == "" || len(s.ID) > 256 || s.Licensee == "" || len(s.Licensee) > 4096 || s.Nodes < 1 || s.ExpiresAt == nil || s.GraceEndsAt == nil || !s.Plan.Valid() || len(s.Features) > len(license.Features()) {
		return "", time.Time{}, ErrContinuity
	}
	features := slices.Clone(s.Features)
	slices.Sort(features)
	for i, f := range features {
		if !slices.Contains(license.Features(), f) || (i > 0 && features[i-1] == f) {
			return "", time.Time{}, ErrContinuity
		}
	}
	// Keep this fixed struct/domain stable: it is the v1 policy representation.
	data, err := json.Marshal(struct {
		Domain, Issuer, ID, Subject string
		Plan                        license.Plan
		Nodes                       int
		Features                    []license.Feature
		ExpiresAt, GraceEndsAt      int64
	}{"kudurru-management-profile/v1", license.Issuer, s.ID, s.Licensee, s.Plan, s.Nodes, features, s.ExpiresAt.Unix(), s.GraceEndsAt.Unix()})
	if err != nil {
		return "", time.Time{}, ErrContinuity
	}
	return hash(data), s.GraceEndsAt.UTC(), nil
}

// NewPending creates NO grant, receipt or durable state. It checks only the
// ORIGINAL carried opaque writer token's target, never samples Store authority.
// Root invokes it after actual withdrawal/floors commit under exclusion, retains
// the original writer context and admits verified candidates in a later command.
// A string becomes reusable cluster evidence only in committed active receipts.
// Even an old carried token can construct a pending string; actual current writer
// admission and final checks remain mandatory root responsibilities.
func NewPending(ctx context.Context, target serving.TermActivation, profile string) (string, error) {
	if ctx == nil || ctx.Err() != nil || target.Term.Validate() != nil || target.Incarnation == 0 || !canonicalHex(profile, 64) {
		return "", ErrContinuity
	}
	token, ok := sqlite.WriterTokenFromContext(ctx)
	if !ok || !token.Term().Same(target.Term) || token.Incarnation() != target.Incarnation {
		return "", ErrContinuity
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", ErrContinuity
	}
	return entitlementPrefix + hash([]byte(target.Term.ClusterID)) + "/" + profile + "/" + hex.EncodeToString(nonce[:]), nil
}

// Parse checks the bounded v1 structure and expected actual cluster/profile.
// It does not prove receipt admission; a valid pending string alone grants nothing.
func Parse(value, clusterID, profile string) (string, error) {
	if len(clusterID) == 0 || len(clusterID) > 256 || !canonicalHex(profile, 64) || len(value) != len(entitlementPrefix)+64+1+64+1+32 {
		return "", ErrContinuity
	}
	parts := strings.Split(value, "/")
	if len(parts) != 5 || parts[0] != "entitlement" || parts[1] != "v1" || parts[2] != hash([]byte(clusterID)) || parts[3] != profile || !canonicalHex(parts[4], 32) {
		return "", ErrContinuity
	}
	return value, nil
}

// Installed selects ONE actual active activation from a validated registry. It
// binds both the registry's actual cluster and every active receipt's cluster to
// the expected cluster/profile; missing, mixed or unknown receipts deny.
//
// Root additionally enforces follower restart/loss fences and exact source,
// credential and trust resource matching. This helper does not clear a fence,
// fabricate a receipt, or infer authority from local license/registry epoch equality.
func Installed(r *providerregistry.Registry, clusterID, profile string) (string, error) {
	if r == nil || r.Digest() == "" || r.ClusterID() != clusterID {
		return "", ErrContinuity
	}
	entitlement := ""
	for _, authority := range r.Authorities() {
		if !authority.Active {
			continue
		}
		value, err := Parse(authority.Binding.EntitlementIdentity, clusterID, profile)
		if err != nil || (entitlement != "" && entitlement != value) {
			return "", ErrContinuity
		}
		entitlement = value
	}
	if entitlement == "" {
		return "", ErrContinuity
	}
	return entitlement, nil
}
