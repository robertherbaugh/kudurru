package dynamicupdate

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/miekg/dns"
)

// MaxPolicyBytes is the finite workspace one zone's update policy may use,
// whatever the aggregate limit.
const MaxPolicyBytes int64 = 4 << 20

// GrantSpec is one configured update grant as an operator writes it (a
// DNSZone's spec.policy.updatePolicies entry): a TSIG key name, an owner name
// or "*." wildcard inside the zone, and record type names ("ANY" or none for
// every type).
type GrantSpec struct {
	KeyRef string
	Name   string
	Types  []string
}

// GrantSpecError is why a configured update policy cannot be compiled. Index
// is the entry (-1 for the policy as a whole) and Field the entry's field
// ("tsigKeyRef", "name", "types"). Capacity marks a bound rather than a value
// that is wrong.
type GrantSpecError struct {
	Index    int
	Field    string
	Value    string
	Detail   string
	Capacity bool
}

func (e *GrantSpecError) Error() string {
	if e.Index < 0 {
		return "update policy: " + e.Detail
	}
	return fmt.Sprintf("update policy %d %s %q: %s", e.Index, e.Field, e.Value, e.Detail)
}

// PreflightGrantSpecs checks the count and length bounds of a configured
// policy before any name is canonicalized, charging budget for the
// workspace the compilation needs.
func PreflightGrantSpecs(specs []GrantSpec, budget int64) *GrantSpecError {
	if len(specs) > MaxPolicyEntries {
		return &GrantSpecError{Index: -1, Capacity: true, Detail: fmt.Sprintf("at most %d entries", MaxPolicyEntries)}
	}
	left, typesLeft := budget, MaxTotalGrantTypes
	charge := func(n int64) bool {
		if n > left {
			return false
		}
		left -= n
		return true
	}
	over := &GrantSpecError{Index: -1, Capacity: true, Detail: fmt.Sprintf("the policy needs more than its %d-byte workspace", budget)}
	for i, g := range specs {
		switch {
		case len(g.KeyRef) > MaxNameBytes:
			return &GrantSpecError{Index: i, Field: "tsigKeyRef", Value: g.KeyRef[:64] + "...", Capacity: true, Detail: "too long"}
		case len(g.Name) > MaxNameBytes+2:
			return &GrantSpecError{Index: i, Field: "name", Value: g.Name[:64] + "...", Capacity: true, Detail: "too long"}
		case len(g.Types) > typesLeft:
			return &GrantSpecError{Index: i, Field: "types", Capacity: true, Detail: fmt.Sprintf("the policy names more than %d types", MaxTotalGrantTypes)}
		}
		typesLeft -= len(g.Types)
		if !charge(256 + int64(len(g.KeyRef))*4 + int64(len(g.Name))*4 + int64(len(g.Types))*32) {
			return over
		}
		for _, typ := range g.Types {
			if len(typ) > 16 {
				return &GrantSpecError{Index: i, Field: "types", Value: typ[:16] + "...", Capacity: true, Detail: "too long"}
			}
			if !charge(int64(len(typ))) {
				return over
			}
		}
	}
	return nil
}

// CompileGrantSpec compiles entry i of a configured policy for the zone
// apex (canonical): the canonical key name, the canonical owner ("*."
// prefixed for a wildcard) and the sorted, deduplicated types, nil for every
// type. keyName is the caller's normalization of a key reference (the
// TSIG keyring's), applied before the key is canonicalized.
func CompileGrantSpec(apex string, i int, g GrantSpec, keyName func(string) string) (Grant, *GrantSpecError) {
	key, err := CanonicalName(keyName(g.KeyRef))
	if err != nil || key == "." || g.KeyRef == "" {
		return Grant{}, &GrantSpecError{Index: i, Field: "tsigKeyRef", Value: g.KeyRef, Detail: "must be the name of a TSIG key"}
	}
	owner := g.Name
	wild := strings.HasPrefix(owner, "*.")
	if wild {
		owner = strings.TrimPrefix(owner, "*.")
	}
	owner, err = CanonicalName(owner)
	if err != nil || strings.Contains(owner, "*") || !dns.IsSubDomain(apex, owner) {
		return Grant{}, &GrantSpecError{Index: i, Field: "name", Value: g.Name, Detail: "must be an absolute name or a *. wildcard at or below the zone " + apex}
	}
	if wild {
		owner = "*." + owner
	}
	var types []uint16
	anyType := false
	for _, name := range g.Types {
		typ, ok := GrantType(name)
		if !ok {
			return Grant{}, &GrantSpecError{Index: i, Field: "types", Value: name, Detail: "must be a record type that an update can carry, or ANY"}
		}
		if typ == dns.TypeANY {
			anyType = true
		} else {
			types = append(types, typ)
		}
	}
	if anyType {
		types = nil
	} else {
		sort.Slice(types, func(i, j int) bool { return types[i] < types[j] })
		n := 0
		for _, typ := range types {
			if n == 0 || types[n-1] != typ {
				types[n] = typ
				n++
			}
		}
		types = types[:n]
	}
	return Grant{Key: key, Owner: owner, Types: types}, nil
}

// GrantType resolves a grant's record type name: a mnemonic or TYPEnnn of a
// type an update may carry, or ANY.
func GrantType(name string) (uint16, bool) {
	name = strings.ToUpper(name)
	typ, ok := dns.StringToType[name]
	if !ok && strings.HasPrefix(name, "TYPE") {
		n, err := strconv.ParseUint(strings.TrimPrefix(name, "TYPE"), 10, 16)
		if err == nil {
			typ, ok = uint16(n), true
		}
	}
	if !ok || (typ != dns.TypeANY && (typ == 0 || typ == 65535 || typ == dns.TypeOPT || (typ >= dns.TypeTKEY && typ <= dns.TypeANY))) {
		return 0, false
	}
	return typ, true
}
