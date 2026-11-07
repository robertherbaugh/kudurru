package cloudflare

import (
	"encoding/json"
	"strings"

	"github.com/miekg/dns"
	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/plan"
)

const markerPrefix = "kudurru:"

func marker(owner kinds.Owner) string {
	b, _ := json.Marshal([]string{owner.ClusterID, owner.UID})
	return markerPrefix + kinds.Digest(b)
}
func stampComment(comment string, owner kinds.Owner) string {
	if comment == "" {
		return marker(owner)
	}
	return comment + " " + marker(owner)
}
func commentOwner(comment string, owner kinds.Owner) (plan.RemoteOwner, string) {
	// Our marker is always a suffix. Preserve the human comment byte for byte;
	// normalizing whitespace would make Preview and subsequent observations drift.
	token := marker(owner)
	if comment == token {
		return plan.RemoteOwner{ClusterID: owner.ClusterID, IntegrationUID: owner.UID}, ""
	}
	if strings.Count(comment, markerPrefix) == 1 && strings.HasSuffix(comment, " "+token) {
		return plan.RemoteOwner{ClusterID: owner.ClusterID, IntegrationUID: owner.UID}, strings.TrimSuffix(comment, " "+token)
	}
	if !strings.Contains(comment, markerPrefix) {
		return plan.RemoteOwner{}, comment
	}
	return plan.RemoteOwner{ClusterID: "foreign", IntegrationUID: "marker"}, comment
}

func heritage(content string) (string, bool) {
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

// attachHeritage supports the documented same-name registry and %{record_type}-
// mapping. Arbitrary custom prefixes/suffixes/encryption are not inferred.
func attachHeritage(rows []plan.RemoteRecord) error {
	type claim struct{ owner, name, typ string }
	var claims []claim
	for i := range rows {
		row := &rows[i]
		if row.Endpoint.RecordType != "TXT" {
			continue
		}
		owner, ok := heritage(row.Endpoint.Targets[0])
		if !ok {
			continue
		}
		if row.Owner != (plan.RemoteOwner{}) {
			continue
		}
		row.Owner = plan.RemoteOwner{ExternalDNSOwner: owner}
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
			if r.Owner == (plan.RemoteOwner{}) {
				r.Owner.ExternalDNSOwner = c.owner
			} else if r.Owner.ExternalDNSOwner != "" && r.Owner.ExternalDNSOwner != c.owner {
				r.Owner = plan.RemoteOwner{ClusterID: "foreign", IntegrationUID: "conflicting-heritage"}
			}
		}
		if !matched {
			return ErrInvalid
		}
	}
	return nil
}
