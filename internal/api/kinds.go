package api

import (
	"slices"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// kindInfo maps a REST kind (the lower-case plural resource name) onto
// the store kind and the RBAC resource that guards it.
type kindInfo struct {
	plural     string
	kind       store.Kind
	resource   string
	namespaced bool
	newObject  func() store.Object
}

// kinds are the object kinds served under /objects/{kind}.
var kinds = []kindInfo{
	{plural: "dnszones", kind: "DNSZone", resource: "zones", newObject: func() store.Object { return &v1alpha1.DNSZone{} }},
	{plural: "dnsrecordsets", kind: "DNSRecordSet", resource: "records", namespaced: true, newObject: func() store.Object { return &v1alpha1.DNSRecordSet{} }},
	{plural: "blocklists", kind: "BlockList", resource: "blocklists", namespaced: true, newObject: func() store.Object { return &v1alpha1.BlockList{} }},
	{plural: "clientgroups", kind: "ClientGroup", resource: "clientgroups", namespaced: true, newObject: func() store.Object { return &v1alpha1.ClientGroup{} }},
	{plural: "policies", kind: "Policy", resource: "policies", namespaced: true, newObject: func() store.Object { return &v1alpha1.Policy{} }},
	{plural: "dhcpscopes", kind: "DHCPScope", resource: "dhcp", newObject: func() store.Object { return &v1alpha1.DHCPScope{} }},
}

func kindByPlural(plural string) (kindInfo, bool) {
	i := slices.IndexFunc(kinds, func(k kindInfo) bool { return k.plural == plural })
	if i < 0 {
		return kindInfo{}, false
	}
	return kinds[i], true
}

func kindByKind(kind store.Kind) (kindInfo, bool) {
	i := slices.IndexFunc(kinds, func(k kindInfo) bool { return k.kind == kind })
	if i < 0 {
		return kindInfo{}, false
	}
	return kinds[i], true
}

// kindPlurals lists the REST kinds, for enums.
func kindPlurals() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.plural
	}
	return out
}

// kindResources lists the RBAC resources of every kind, for the lint.
func kindResources() []string {
	out := make([]string, len(kinds))
	for i, k := range kinds {
		out[i] = k.resource
	}
	return out
}

// zoneScoped reports whether objects of the kind are authorized at the
// scope of their zone (zone-admin bindings), so list routes filter instead
// of refusing.
func (k kindInfo) zoneScoped() bool { return k.plural == "dnszones" || k.plural == "dnsrecordsets" }
