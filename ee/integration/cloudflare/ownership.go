package cloudflare

import (
	"encoding/json"
	"strings"

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

// attachHeritage applies the shared external-dns registry mapping (D380).
func attachHeritage(rows []plan.RemoteRecord) error {
	if plan.AttachHeritage(rows) != nil {
		return ErrInvalid
	}
	return nil
}
