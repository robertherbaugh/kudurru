package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func bounded(s string, limit int) bool {
	return s != "" && len(s) <= limit && strings.TrimSpace(s) == s && !strings.ContainsFunc(s, unicode.IsControl)
}

// Validate rejects malformed or unbounded Owner state.
func (o Owner) Validate() error {
	if !bounded(o.ClusterID, 256) || !bounded(o.UID, 256) || len(validation.IsDNS1123Label(o.Namespace)) != 0 || len(validation.IsDNS1123Subdomain(o.Name)) != 0 {
		return fmt.Errorf("invalid integration owner identity")
	}
	return nil
}

// Validate rejects malformed or unbounded Destination state.
func (d Destination) Validate() error {
	if !bounded(d.Provider, 64) || !bounded(d.Account, 256) || !bounded(d.Zone, 256) {
		return fmt.Errorf("invalid integration destination")
	}
	return nil
}

// Validate rejects malformed or unbounded RecordKey state.
func (k RecordKey) Validate() error {
	_, ok := dns.IsDomainName(k.Name)
	if !ok || !bounded(k.Name, 254) || k.Name != dns.CanonicalName(k.Name) || len(k.SetIdentifier) > 256 || strings.ContainsFunc(k.SetIdentifier, unicode.IsControl) || !validType(k.Type) {
		return fmt.Errorf("invalid integration record key")
	}
	return nil
}
func validType(typ string) bool {
	if n, ok := dns.StringToType[typ]; ok {
		return n != dns.TypeOPT && n != dns.TypeTSIG && n != dns.TypeTKEY && n != dns.TypeAXFR && n != dns.TypeIXFR && n != dns.TypeANY && n != dns.TypeNone && n != dns.TypeMAILA && n != dns.TypeMAILB
	}
	if !strings.HasPrefix(typ, "TYPE") {
		return false
	}
	rr, err := dns.NewRR("example. 1 IN " + typ + " \\# 0")
	return err == nil && rr.Header().Rrtype != 0 && dns.TypeToString[rr.Header().Rrtype] == "" && typ == fmt.Sprintf("TYPE%d", rr.Header().Rrtype)
}

// Validate rejects malformed or unbounded RecordIdentity state.
func (r RecordIdentity) Validate() error {
	if err := r.Owner.Validate(); err != nil {
		return err
	}
	if err := r.Destination.Validate(); err != nil {
		return err
	}
	return r.Key.Validate()
}
func objectValid(v any, version int, name, namespace, expected string) error {
	if version != FormatVersion || namespace != "" || name != expected || expected == "" {
		return fmt.Errorf("invalid integration runtime name/version/scope")
	}
	b, err := json.Marshal(v)
	if err != nil || len(b) > MaxObjectBytes {
		return fmt.Errorf("integration runtime object exceeds encoding bound")
	}
	return nil
}
func policyValid(p string) bool {
	return p == "" || p == "sync" || p == "upsert-only" || p == "create-only"
}
func optionalDigest(s string) bool { return s == "" || ValidDigest(s) }
func idsValid(ids []string) bool {
	if len(ids) > MaxTargets {
		return false
	}
	seen := map[string]bool{}
	for _, id := range ids {
		if !bounded(id, 512) || seen[id] {
			return false
		}
		seen[id] = true
	}
	return true
}

// EndpointDigest fingerprints canonical content; nil represents absence.
func EndpointDigest(e *public.Endpoint) string {
	if e == nil {
		return ""
	}
	b, _ := json.Marshal(e)
	return Digest(b)
}
func endpointValid(e *public.Endpoint, k RecordKey) bool {
	if e == nil || e.DNSName != k.Name || e.RecordType != k.Type || e.SetIdentifier != k.SetIdentifier || len(e.Targets) == 0 || len(e.Targets) > MaxTargets || e.RecordTTL < 0 || e.RecordTTL > 2147483647 || len(e.ProviderSpecific) > 64 || len(e.Labels) > 64 {
		return false
	}
	for _, target := range e.Targets {
		if !bounded(target, 8192) {
			return false
		}
		rr, err := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", e.DNSName, e.RecordTTL, e.RecordType, target))
		if err != nil || rr.Header().Name != e.DNSName || rr.Header().Class != dns.ClassINET {
			return false
		}
		if _, err := dns.PackRR(rr, make([]byte, dns.Len(rr)), 0, nil, false); err != nil {
			return false
		}
	}
	return true
}

// Validate rejects malformed or unbounded IntegrationLedger state.
func (s *IntegrationLedger) Validate() error {
	if s == nil {
		return fmt.Errorf("nil integration ledger")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, LedgerName(p.Record)); err != nil {
		return err
	}
	if err := p.Record.Validate(); err != nil {
		return err
	}
	if !policyValid(p.Policy) || p.Sequence == 0 || !optionalDigest(p.ContentHash) || !optionalDigest(p.LastOperationID) || !idsValid(p.ProviderIDs) || p.ContentHash == "" && len(p.ProviderIDs) > 0 || p.LastApplied.IsZero() != (p.LastOperationID == "") {
		return fmt.Errorf("invalid integration ledger baseline")
	}
	if p.Pending != nil && (!ValidDigest(p.Pending.OperationID) || !ValidDigest(p.Pending.PlanDigest) || !optionalDigest(p.Pending.BeforeHash) || !optionalDigest(p.Pending.AfterHash) || p.Pending.BeforeHash != p.ContentHash) {
		return fmt.Errorf("invalid integration ledger pending operation")
	}
	return nil
}

// Validate rejects malformed or unbounded IntegrationOperation state.
func (s *IntegrationOperation) Validate() error {
	if s == nil {
		return fmt.Errorf("nil integration operation")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, OperationName(p.OperationID)); err != nil {
		return err
	}
	if err := p.Record.Validate(); err != nil {
		return err
	}
	if !policyValid(p.Policy) || !ValidDigest(p.OperationID) || !ValidDigest(p.PlanDigest) || !ValidDigest(p.ConfigDigest) || !ValidDigest(p.CredentialDigest) || p.CreatedAt.IsZero() || !idsValid(p.ProviderIDs) || !idsValid(p.RemoteVersions) || EndpointDigest(p.Before) != p.BeforeHash || EndpointDigest(p.After) != p.AfterHash {
		return fmt.Errorf("invalid integration operation digests/identity")
	}
	if p.Before != nil && !endpointValid(p.Before, p.Record.Key) || p.After != nil && !endpointValid(p.After, p.Record.Key) {
		return fmt.Errorf("invalid integration operation endpoint")
	}
	switch p.Action {
	case "create":
		if p.Before != nil || p.After == nil {
			return fmt.Errorf("invalid create")
		}
	case "update":
		if p.Before == nil || p.After == nil || p.BeforeHash == p.AfterHash {
			return fmt.Errorf("invalid update")
		}
	case "adopt":
		if p.Before == nil || p.After == nil {
			return fmt.Errorf("invalid adopt")
		}
	case "delete":
		if p.Before == nil || p.After != nil {
			return fmt.Errorf("invalid delete")
		}
	default:
		return fmt.Errorf("invalid integration operation action")
	}
	if !p.AbandonedAt.IsZero() && p.AbandonedAt.Before(p.CreatedAt) {
		return fmt.Errorf("invalid abandoned operation time")
	}
	if p.Receipt != nil {
		r := p.Receipt
		if r.ObservedAt.IsZero() || r.ObservedAt.Before(p.CreatedAt) || !optionalDigest(r.ContentHash) || !idsValid(r.ProviderIDs) || len(r.Targets) > MaxTargets || len(r.ErrorClass) > 64 {
			return fmt.Errorf("invalid integration receipt")
		}
		if r.Complete && (len(r.ProviderIDs) == 0 && p.After != nil || !p.AbandonedAt.IsZero() || r.ContentHash != p.AfterHash || r.ErrorClass != "" || p.After == nil && len(r.ProviderIDs) > 0) {
			return fmt.Errorf("receipt does not confirm operation")
		}
		seenTargets := map[string]bool{}
		for _, t := range r.Targets {
			if seenTargets[t.Target] || !p.AbandonedAt.IsZero() && t.Applied || !bounded(t.Target, 8192) || len(t.ProviderID) > 512 || len(t.ErrorClass) > 64 || t.Applied && t.ErrorClass != "" {
				return fmt.Errorf("invalid integration target receipt")
			}
			seenTargets[t.Target] = true
		}
	}
	return nil
}

// Validate rejects malformed or unbounded IntegrationBudget state.
func (s *IntegrationBudget) Validate() error {
	if s == nil {
		return fmt.Errorf("nil integration budget")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, BudgetName(p.ClusterID, p.Provider, p.Account)); err != nil {
		return err
	}
	if !bounded(p.ClusterID, 256) || !bounded(p.Provider, 64) || !bounded(p.Account, 256) || p.Sequence == 0 || p.WindowStart.IsZero() || p.WindowSeconds == 0 || p.WindowSeconds > 3600 || p.Capacity == 0 || p.Capacity > 100000 || len(p.Reservations) > 10000 {
		return fmt.Errorf("invalid integration budget")
	}
	seen := map[string]bool{}
	total := uint64(0)
	for _, r := range p.Reservations {
		if !ValidDigest(r.ID) || r.Cost == 0 || seen[r.ID] {
			return fmt.Errorf("invalid budget reservation")
		}
		seen[r.ID] = true
		total += uint64(r.Cost)
	}
	if total > uint64(p.Capacity) {
		return fmt.Errorf("integration budget exceeded")
	}
	return nil
}

// Validate rejects malformed or unbounded IntegrationDelivery state.
func (s *IntegrationDelivery) Validate() error {
	if s == nil {
		return fmt.Errorf("nil integration delivery")
	}
	p := s.Spec
	if err := objectValid(s, p.FormatVersion, s.Name, s.Namespace, DeliveryName(p.Owner, p.DeliveryID)); err != nil {
		return err
	}
	if err := p.Owner.Validate(); err != nil {
		return err
	}
	if !ValidDigest(p.DeliveryID) || !bounded(p.Topic, 128) || len(p.Payload) == 0 || len(p.Payload) > MaxPayloadBytes || !json.Valid(p.Payload) || p.CreatedAt.IsZero() || !p.Deadline.After(p.CreatedAt) || p.Deadline.Sub(p.CreatedAt) > 24*time.Hour || p.NextAttempt.Before(p.CreatedAt) || p.NextAttempt.After(p.Deadline) || p.Attempts > 10000 || p.Acknowledged && p.DeadLetter || len(p.ErrorClass) > 64 {
		return fmt.Errorf("invalid integration delivery")
	}
	return nil
}
