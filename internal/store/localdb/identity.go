package localdb

import "errors"

// CheckClusterIdentity admits saved state only against an independent pin.
// observed is optional during an outage, but can never override expected.
// An old snapshot without identity must be replaced by a verified live sync.
// Errors intentionally exclude identity values and saved object contents.
func CheckClusterIdentity(expected, observed, saved string) error {
	if expected == "" {
		return errors.New("expected cluster identity is missing; provision store.kube.expectedClusterID before offline recovery")
	}
	if observed != "" && observed != expected {
		return errors.New("observed cluster identity differs from the expected cluster identity")
	}
	if saved == "" {
		return errors.New("saved cluster identity is missing; an online sync is required to upgrade saved state")
	}
	if saved != expected {
		return errors.New("saved cluster identity differs from the expected cluster identity")
	}
	return nil
}
