package servingbuild

import (
	"context"

	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/serving"
	"github.com/robertherbaugh/kudurru/internal/store/sqlite"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/accepted"
	"github.com/robertherbaugh/kudurru/internal/trustanchors/resolverstate"
)

// prepareAnchorInput never accepts a proposed Projection as durable evidence.
// The original candidate must agree with the actual Store-issued publication;
// publication still compares that same token at the final complete swap.
func prepareAnchorInput(ctx context.Context, candidate serving.Candidate, cfg *config.Config, source *sqlite.Store) (*sqlite.AnchorPublication, error) {
	if candidate.Image == nil || cfg == nil {
		return nil, serving.ErrComponents
	}
	policy, err := cfg.Upstreams.Recursion.RootAnchorPolicy()
	if err != nil {
		return nil, err
	}
	owner, err := accepted.DecodeRows(candidate.Image.Payload.Runtime, candidate.Image.Term.ClusterID)
	if err != nil {
		return nil, err
	}
	if policy.Mode == config.RootAnchorsStatic {
		if owner.Inspect().Phase != accepted.Dormant {
			return nil, sqlite.ErrAnchorUnavailable
		}
		return nil, nil
	}
	if source == nil {
		return nil, sqlite.ErrAnchorUnavailable
	}
	publication, err := source.CaptureAnchorPublication(ctx, candidate.Token)
	if err != nil {
		return nil, err
	}
	projection := publication.Projection()
	identity := projection.Identity()
	if projection.Disposition() != resolverstate.Usable || len(projection.DS()) == 0 ||
		identity.ClusterID != candidate.Image.Term.ClusterID || identity.Owner != owner.Version() ||
		identity.PolicyID != policy.PolicyID || identity.SeedDigest != policy.SeedDigest ||
		!publication.Input().Current() || publication.Input().CacheDomain() == ([32]byte{}) {
		return nil, sqlite.ErrAnchorUnavailable
	}
	return publication, nil
}
