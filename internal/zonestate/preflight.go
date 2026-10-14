package zonestate

import (
	"fmt"

	"github.com/robertherbaugh/kudurru/internal/dynamicupdate"
)

// aggregatePreflight accounts for ALL variable data and metadata workspace
// before normalization copies anything. It is a conservative allocation charge,
// not an assertion about the Go runtime's total RSS. Encoded output and raw RR
// wire retain their own exact bounds. Fixed charges bound slice/map work too.
func aggregatePreflight(a *Aggregate, l Limits) error {
	if len(a.Records) > l.MaxRecords || len(a.Sources) > l.MaxRecords || len(a.Reservations) > l.MaxRecords || len(a.Receipts) > l.MaxReceipts {
		return ErrCapacity
	}
	left := l.MaxAggregateBytes
	charge := func(n int64) bool {
		if n < 0 || n > left {
			return false
		}
		left -= n
		return true
	}
	for _, n := range []int{len(a.Config), len(a.Zone.ClusterID), len(a.Zone.ObjectName), len(a.Zone.UID), len(a.Zone.Apex)} {
		if !charge(int64(n)) {
			return ErrCapacity
		}
	}
	if !charge(int64(len(a.Sources))*128) || !charge(int64(len(a.Records))*192) || !charge(int64(len(a.Receipts))*256) {
		return ErrCapacity
	}
	for _, s := range a.Sources {
		for _, str := range []string{s.ID, s.UID, s.ResourceVersion, s.Manager} {
			if len(str) > dynamicupdate.MaxIdentityBytes || !charge(int64(len(str))) {
				return fmt.Errorf("%w: source string", ErrCapacity)
			}
		}
		if !charge(int64(len(s.Desired))) {
			return ErrCapacity
		}
	}
	var wireLeft = l.MaxWireBytes
	for _, r := range a.Records {
		if len(r.Wire) > 65535 || int64(len(r.Wire)) > wireLeft || !charge(int64(len(r.Wire))) {
			return fmt.Errorf("%w: record wire", ErrCapacity)
		}
		wireLeft -= int64(len(r.Wire))
		if len(r.Provenance.Key) > dynamicupdate.MaxNameBytes {
			return fmt.Errorf("%w: provenance key", ErrCapacity)
		}
		for _, str := range []string{r.Provenance.SourceID, r.Provenance.CredentialVersion} {
			if len(str) > dynamicupdate.MaxIdentityBytes || !charge(int64(len(str))) {
				return fmt.Errorf("%w: provenance string", ErrCapacity)
			}
		}
		if !charge(int64(len(r.Provenance.Key)) * 4) {
			return ErrCapacity
		}
	}
	for _, r := range a.Receipts {
		if len(r.SemanticDigest) > 64 || len(r.Principal.Key) > dynamicupdate.MaxNameBytes {
			return fmt.Errorf("%w: receipt digest/key", ErrCapacity)
		}
		for _, str := range []string{r.ID, r.Principal.Version} {
			if len(str) > dynamicupdate.MaxIdentityBytes || !charge(int64(len(str))) {
				return fmt.Errorf("%w: receipt string", ErrCapacity)
			}
		}
		if !charge(int64(len(r.SemanticDigest))) || !charge(int64(len(r.Principal.Key))*4) {
			return ErrCapacity
		}
	}
	if _, err := dynamicupdate.PolicyAllocation(a.Policy.Credentials, a.Policy.Grants, a.Reservations, left); err != nil {
		return fmt.Errorf("%w: %v", ErrCapacity, err)
	}
	return nil
}
