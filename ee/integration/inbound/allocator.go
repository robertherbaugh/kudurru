// Package inbound prepares bounded Integration source images without I/O or
// mutation authority. Production adapters must fence and persist its results.
package inbound

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// NameRequest asks for a stable relative label for one immutable source client.
type NameRequest struct{ ID, Base string }

// NameReservation protects operator/UPDATE names or whole DNAME/child subtrees.
// Inputs are current foreign reservations, never inferred from annotations.
type NameReservation struct {
	Name    string
	Subtree bool
}

// AllocationInput is a complete bounded cluster allocation cut and target scope.
type AllocationInput struct {
	Owner        kinds.Owner
	Scope        kinds.AllocationScope
	Requests     []NameRequest
	Existing     []*kinds.IntegrationNameAllocation
	Reservations []NameReservation
	At           time.Time
}

// AllocationResult separates unchanged durable assignments from create-only rows.
// New rows have no fabricated UID/RV and confer no authority until committed.
type AllocationResult struct {
	Labels map[string]string
	New    []*kinds.IntegrationNameAllocation
}

// AllocatedLabel checks the documented base/base-2 contract including truncation.
func AllocatedLabel(base, label string) bool {
	return kinds.AllocationLabelMatches(base, label)
}
func allocationLabel(base string, n uint32) string {
	if n == 1 {
		return base
	}
	suffix := "-" + strconv.FormatUint(uint64(n), 10)
	if len(base) > 63-len(suffix) {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return base + suffix
}

// AllocateNames validates the entire cut, then allocates sorted IDs atomically.
// Missing clients never free a label; saved assignments never silently resuffix.
func AllocateNames(in AllocationInput) (AllocationResult, error) {
	invalid := func() (AllocationResult, error) {
		return AllocationResult{}, errors.New("inbound: invalid/conflicting allocation cut")
	}
	if in.Owner.Validate() != nil || in.Scope.Validate() != nil || in.Owner.ClusterID != in.Scope.ClusterID || in.At.IsZero() || len(in.Requests) > kinds.MaxSourceAllocations || len(in.Existing) > kinds.MaxClusterAllocations || len(in.Reservations) > kinds.MaxClusterAllocations {
		return invalid()
	}
	occupied := map[string]bool{}
	matched := map[string]*kinds.IntegrationNameAllocation{}
	objects := map[string]bool{}
	bySource := map[kinds.Owner]int{}
	for _, a := range in.Existing {
		if a == nil || a.Validate() != nil || a.UID == "" || a.ResourceVersion == "" || a.Spec.Scope.ClusterID != in.Owner.ClusterID || a.Spec.CreatedAt.After(in.At) || objects[a.Name] || !AllocatedLabel(a.Spec.Base, a.Spec.Label) {
			return invalid()
		}
		objects[a.Name] = true
		bySource[a.Spec.Owner]++
		if bySource[a.Spec.Owner] > kinds.MaxSourceAllocations {
			return invalid()
		}
		if a.Spec.Scope == in.Scope {
			if occupied[a.Spec.Label] {
				return invalid()
			}
			occupied[a.Spec.Label] = true
			if a.Spec.Owner == in.Owner {
				key := a.Spec.ClientID + "\x00" + a.Spec.Base
				if matched[key] != nil {
					return invalid()
				}
				matched[key] = a
			}
		}
	}
	exact, subtrees := map[string]bool{}, map[string]bool{}
	for _, r := range in.Reservations {
		if r.Name != dns.CanonicalName(r.Name) || !safeName(r.Name) {
			return invalid()
		}
		exact[r.Name] = true
		if r.Subtree {
			subtrees[r.Name] = true
		}
	}
	reserved := func(label string) bool {
		name := label + "." + in.Scope.Apex
		if exact[name] {
			return true
		}
		for {
			if subtrees[name] {
				return true
			}
			if name == "." {
				break
			}
			labels := dns.SplitDomainName(name)
			if len(labels) <= 1 {
				name = "."
			} else {
				name = strings.Join(labels[1:], ".") + "."
			}
		}

		return false
	}
	requests := slices.Clone(in.Requests)
	slices.SortFunc(requests, func(a, b NameRequest) int { return strings.Compare(a.ID, b.ID) })
	out := AllocationResult{Labels: map[string]string{}}
	ordinals := map[string]uint32{}
	for _, r := range requests {
		if !boundedText(r.ID, 256) || len(validation.IsDNS1123Label(r.Base)) != 0 || out.Labels[r.ID] != "" {
			return invalid()
		}
		if a := matched[r.ID+"\x00"+r.Base]; a != nil {
			if reserved(a.Spec.Label) {
				return invalid()
			}
			out.Labels[r.ID] = a.Spec.Label
			continue
		}
		label := ""
		// More blocked siblings than this require rejection, never an unbounded scan.
		start := ordinals[r.Base]
		if start == 0 {
			start = 1
		}
		for n := start; n <= kinds.MaxClusterAllocations*2+1; n++ {
			candidate := allocationLabel(r.Base, n)
			if !occupied[candidate] && !reserved(candidate) {
				label = candidate
				ordinals[r.Base] = n + 1
				break
			}
		}
		if label == "" || len(in.Existing)+len(out.New) >= kinds.MaxClusterAllocations || bySource[in.Owner]+len(out.New) >= kinds.MaxSourceAllocations {
			return invalid()
		}
		occupied[label] = true
		out.Labels[r.ID] = label
		a := &kinds.IntegrationNameAllocation{ObjectMeta: metav1.ObjectMeta{Name: kinds.NameAllocationName(in.Scope, label)}, Spec: kinds.IntegrationNameAllocationSpec{FormatVersion: kinds.FormatVersion, Scope: in.Scope, Label: label, Owner: in.Owner, ClientID: r.ID, Base: r.Base, CreatedAt: in.At}}
		if a.Validate() != nil {
			return invalid()
		}
		out.New = append(out.New, a)
	}
	return out, nil
}
