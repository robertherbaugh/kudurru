package servingzones

import (
	"strings"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/primaryruntime"
)

// Legacy nonprimary configurations without a lifecycle or primary ancestor may
// still serve. Coordinated incarnations use only the complete image admission
// predicate, never a public ObjectSet or a transfer identity as lifecycle proof.
func membership(image *primaryruntime.PreparedImage, cut primaryruntime.Cut, config *public.DNSZone, now time.Time) (eligible, coordinated bool) {
	apex := dns.CanonicalName(config.Spec.ZoneName)
	if member, exists := cut.Member(apex); exists {
		return member.Zone.ObjectName == config.Name && member.Zone.UID == string(config.UID) && member.Type == config.Spec.Type && image.Admitted(apex, now), true
	}
	if image.Coordinated(config.Name) {
		return false, true
	}
	labels := dns.SplitDomainName(apex)
	for i := 1; i <= len(labels); i++ {
		ancestor := strings.Join(labels[i:], ".") + "."
		if parent, exists := cut.Member(ancestor); exists && parent.Type == public.ZoneTypePrimary {
			return false, true
		}
	}
	return true, false
}
