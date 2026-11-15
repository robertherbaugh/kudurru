package plan

import (
	"strings"

	"github.com/miekg/dns"
)

// Heritage parses one external-dns TXT registry value, in presentation form,
// and returns the external-dns owner it names. It accepts only the
// documented "heritage=external-dns,external-dns/owner=<owner>,..." shape
// with no repeated key.
func Heritage(content string) (string, bool) {
	rr, err := dns.NewRR("owner.example. 1 IN TXT " + content)
	if err != nil {
		return "", false
	}
	txt, ok := rr.(*dns.TXT)
	if !ok {
		return "", false
	}
	labels := map[string]string{}
	for _, part := range strings.Split(strings.Join(txt.Txt, ""), ",") {
		key, value, ok := strings.Cut(part, "=")
		if !ok || labels[key] != "" {
			return "", false
		}
		labels[key] = value
	}
	if labels["heritage"] != "external-dns" || labels["external-dns/owner"] == "" {
		return "", false
	}
	return labels["external-dns/owner"], true
}

// AttachHeritage marks rows claimed by an external-dns TXT registry with the
// owner it names. It supports the documented same-name registry and the
// %{record_type}- prefix mapping; arbitrary custom prefixes, suffixes and
// encryption are not inferred. A row that already has an owner keeps it,
// and two registries naming different owners for one row make it foreign.
// A registry row that claims no row is ErrInvalid: the snapshot is not one
// this mapping understands, so it must not be planned from.
func AttachHeritage(rows []RemoteRecord) error {
	type claim struct{ owner, name, typ string }
	var claims []claim
	for i := range rows {
		row := &rows[i]
		if row.Endpoint.RecordType != "TXT" || len(row.Endpoint.Targets) != 1 {
			continue
		}
		owner, ok := Heritage(row.Endpoint.Targets[0])
		if !ok {
			continue
		}
		if row.Owner != (RemoteOwner{}) {
			continue
		}
		row.Owner = RemoteOwner{ExternalDNSOwner: owner}
		name := row.Endpoint.DNSName
		typ := ""
		for _, prefix := range []string{"a", "aaaa", "cname", "ns", "mx", "ptr", "srv", "naptr", "txt", "dname", "tlsa"} {
			if strings.HasPrefix(name, prefix+"-") {
				typ = strings.ToUpper(prefix)
				name = strings.TrimPrefix(name, prefix+"-")
				break
			}
		}
		claims = append(claims, claim{owner, name, typ})
	}
	for _, c := range claims {
		matched := false
		for i := range rows {
			r := &rows[i]
			if r.Endpoint.DNSName != c.name || c.typ != "" && r.Endpoint.RecordType != c.typ || r.Endpoint.RecordType == "TXT" && r.Owner.ExternalDNSOwner != "" {
				continue
			}
			matched = true
			if r.Owner == (RemoteOwner{}) {
				r.Owner.ExternalDNSOwner = c.owner
			} else if r.Owner.ExternalDNSOwner != "" && r.Owner.ExternalDNSOwner != c.owner {
				r.Owner = RemoteOwner{ClusterID: "foreign", IntegrationUID: "conflicting-heritage"}
			}
		}
		if !matched {
			return ErrInvalid
		}
	}
	return nil
}
