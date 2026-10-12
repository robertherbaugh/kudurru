package cli

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/duration"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// Kind describes one API object kind for the generic verbs.
type Kind struct {
	// Resource is the lower-case plural used in /objects/{kind}.
	Resource string
	// Kind is the CamelCase kind in manifests.
	Kind string
	// Namespaced is false for cluster-scoped kinds (DNSZone).
	Namespaced bool
	// Aliases are accepted on the command line besides Resource.
	Aliases []string
	// columns returns the headers and a row function for tables.
	columns func(wide bool) ([]string, func(obj json.RawMessage, now time.Time) ([]string, error))
}

// Kinds lists every kind the CLI knows, in backup order (zones before the
// record sets that reference them, groups and lists before policies).
var Kinds = []Kind{
	{Resource: "dnszones", Kind: "DNSZone", Aliases: []string{"dnszone", "zone", "zones", "dz"}, columns: zoneColumns},
	{Resource: "dnsrecordsets", Kind: "DNSRecordSet", Namespaced: true, Aliases: []string{"dnsrecordset", "rrset", "rrsets", "recordset", "recordsets"}, columns: rrsetColumns},
	{Resource: "blocklists", Kind: "BlockList", Namespaced: true, Aliases: []string{"blocklist", "list", "lists", "bl"}, columns: blocklistColumns},
	{Resource: "clientgroups", Kind: "ClientGroup", Namespaced: true, Aliases: []string{"clientgroup", "cg", "cgs"}, columns: clientgroupColumns},
	{Resource: "policies", Kind: "Policy", Namespaced: true, Aliases: []string{"policy", "pol", "pols"}, columns: policyColumns},
}

// LookupKind resolves a resource name, alias or CamelCase kind
// (case-insensitive).
func LookupKind(name string) (Kind, error) {
	n := strings.ToLower(strings.TrimSpace(name))
	// kubectl-style resource.group suffixes ("dnszones.kudurru.io").
	n, _, _ = strings.Cut(n, ".")
	for _, k := range Kinds {
		if n == k.Resource || n == strings.ToLower(k.Kind) {
			return k, nil
		}
		for _, a := range k.Aliases {
			if n == a {
				return k, nil
			}
		}
	}
	return Kind{}, fmt.Errorf("unknown kind %q (known: %s)", name, strings.Join(KindNames(), ", "))
}

// KindNames returns the resource names with their short aliases, for help
// and completion.
func KindNames() []string {
	out := make([]string, 0, len(Kinds))
	for _, k := range Kinds {
		out = append(out, k.Resource)
	}
	return out
}

// KindCompletions returns every accepted kind spelling.
func KindCompletions() []string {
	var out []string
	for _, k := range Kinds {
		out = append(out, k.Resource)
		out = append(out, k.Aliases...)
	}
	sort.Strings(out)
	return out
}

// Meta is the identifying part of any object.
type Meta struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   metav1.ObjectMeta `json:"metadata"`
}

// DecodeMeta reads apiVersion, kind and metadata of a raw object.
func DecodeMeta(obj json.RawMessage) (Meta, error) {
	var m Meta
	err := json.Unmarshal(obj, &m)
	return m, err
}

// ObjectTable renders objects of one kind as a kubectl-style table. With
// allNamespaces a NAMESPACE column comes first for namespaced kinds.
func ObjectTable(k Kind, items []json.RawMessage, wide, allNamespaces bool, now time.Time) (*Table, error) {
	headers, row := k.columns(wide)
	t := &Table{}
	showNS := allNamespaces && k.Namespaced
	if showNS {
		t.Headers = append(t.Headers, "NAMESPACE")
	}
	t.Headers = append(t.Headers, "NAME")
	t.Headers = append(t.Headers, headers...)
	for _, obj := range items {
		m, err := DecodeMeta(obj)
		if err != nil {
			return nil, fmt.Errorf("decode %s: %w", k.Resource, err)
		}
		cells, err := row(obj, now)
		if err != nil {
			return nil, fmt.Errorf("decode %s %s: %w", k.Resource, m.Metadata.Name, err)
		}
		var r []string
		if showNS {
			r = append(r, m.Metadata.Namespace)
		}
		r = append(r, m.Metadata.Name)
		t.Rows = append(t.Rows, append(r, cells...))
	}
	return t, nil
}

// Age formats the time since t like kubectl ("5m", "3d2h"); "<unknown>"
// for a zero time.
func Age(t metav1.Time, now time.Time) string {
	if t.IsZero() {
		return "<unknown>"
	}
	return duration.HumanDuration(now.Sub(t.Time))
}

// ReadyStatus returns the status of the Ready condition, or "Unknown".
func ReadyStatus(conds []metav1.Condition) string {
	for _, c := range conds {
		if c.Type == "Ready" {
			return string(c.Status)
		}
	}
	return "Unknown"
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func joinOrDash(ss []string) string { return dash(strings.Join(ss, ",")) }

func itoa(n int) string { return strconv.Itoa(n) }

// ManagedBy returns the kudurru.io/managed-by annotation ("-" if unset).
func ManagedBy(m metav1.ObjectMeta) string {
	return dash(m.Annotations[v1alpha1.ManagedByAnnotation])
}

func zoneColumns(wide bool) ([]string, func(json.RawMessage, time.Time) ([]string, error)) {
	h := []string{"ZONE", "TYPE", "SERIAL", "RECORDS", "READY", "MANAGED-BY", "AGE"}
	if wide {
		h = append(h, "NAMESERVERS", "CONTENT-HASH")
	}
	return h, func(raw json.RawMessage, now time.Time) ([]string, error) {
		var z v1alpha1.DNSZone
		if err := json.Unmarshal(raw, &z); err != nil {
			return nil, err
		}
		r := []string{z.Spec.ZoneName, string(z.Spec.Type), strconv.FormatUint(uint64(z.Status.Serial), 10),
			itoa(z.Status.RecordCount), ReadyStatus(z.Status.Conditions), ManagedBy(z.ObjectMeta), Age(z.CreationTimestamp, now)}
		if wide {
			hash := z.Status.ContentHash
			if len(hash) > 12 {
				hash = hash[:12]
			}
			r = append(r, joinOrDash(z.Spec.Nameservers), dash(hash))
		}
		return r, nil
	}
}

func rrsetColumns(wide bool) ([]string, func(json.RawMessage, time.Time) ([]string, error)) {
	h := []string{"ZONE", "NAMES", "READY", "AGE"}
	if wide {
		h = append(h, "ENDPOINTS", "MANAGED-BY")
	}
	return h, func(raw json.RawMessage, now time.Time) ([]string, error) {
		var s v1alpha1.DNSRecordSet
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		r := []string{s.Spec.ZoneRef.Name, itoa(s.Status.Names), ReadyStatus(s.Status.Conditions), Age(s.CreationTimestamp, now)}
		if wide {
			r = append(r, itoa(len(s.Spec.Endpoints)), ManagedBy(s.ObjectMeta))
		}
		return r, nil
	}
}

func blocklistColumns(wide bool) ([]string, func(json.RawMessage, time.Time) ([]string, error)) {
	h := []string{"MODE", "ENTRIES", "LAST-FETCH", "READY", "AGE"}
	if wide {
		h = append(h, "SOURCES", "RULES", "INVALID", "MANAGED-BY")
	}
	return h, func(raw json.RawMessage, now time.Time) ([]string, error) {
		var b v1alpha1.BlockList
		if err := json.Unmarshal(raw, &b); err != nil {
			return nil, err
		}
		mode := string(b.Spec.Mode)
		if mode == "" {
			mode = "block"
		}
		last := "-"
		if b.Status.LastFetched != nil {
			last = Age(*b.Status.LastFetched, now)
		}
		r := []string{mode, itoa(b.Status.Entries), last, ReadyStatus(b.Status.Conditions), Age(b.CreationTimestamp, now)}
		if wide {
			r = append(r, itoa(len(b.Spec.Sources)), itoa(b.Status.RuleCount), itoa(b.Status.Invalid), ManagedBy(b.ObjectMeta))
		}
		return r, nil
	}
}

func clientgroupColumns(wide bool) ([]string, func(json.RawMessage, time.Time) ([]string, error)) {
	h := []string{"MEMBERS", "READY", "AGE"}
	if wide {
		h = append(h, "MATCHERS", "MANAGED-BY")
	}
	return h, func(raw json.RawMessage, now time.Time) ([]string, error) {
		var g v1alpha1.ClientGroup
		if err := json.Unmarshal(raw, &g); err != nil {
			return nil, err
		}
		r := []string{itoa(g.Status.Members), ReadyStatus(g.Status.Conditions), Age(g.CreationTimestamp, now)}
		if wide {
			r = append(r, itoa(len(g.Spec.Clients)), ManagedBy(g.ObjectMeta))
		}
		return r, nil
	}
}

func policyColumns(wide bool) ([]string, func(json.RawMessage, time.Time) ([]string, error)) {
	h := []string{"GROUPS", "PRIORITY", "READY", "AGE"}
	if wide {
		h = append(h, "BLOCKLISTS", "ALLOWLISTS", "UPSTREAM", "MANAGED-BY")
	}
	return h, func(raw json.RawMessage, now time.Time) ([]string, error) {
		var p v1alpha1.Policy
		if err := json.Unmarshal(raw, &p); err != nil {
			return nil, err
		}
		r := []string{joinOrDash(p.Spec.ClientGroupRefs), itoa(p.Spec.Priority), ReadyStatus(p.Status.Conditions), Age(p.CreationTimestamp, now)}
		if wide {
			r = append(r, joinOrDash(p.Spec.BlockListRefs), joinOrDash(p.Spec.AllowListRefs), dash(p.Spec.UpstreamGroup), ManagedBy(p.ObjectMeta))
		}
		return r, nil
	}
}
