package unifi

import (
	"encoding/hex"
	"net/netip"
	"strconv"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/ee/integration/host"
)

func reverseApices(prefix netip.Prefix) ([]string, error) {
	if !prefix.IsValid() || prefix.Bits() <= 0 || prefix.Addr().Is4In6() {
		return nil, ErrInvalid
	}
	prefix = prefix.Masked()
	unit, bits := 4, prefix.Bits()
	if prefix.Addr().Is4() {
		unit = 8
		bits = min(bits, 24)
	}
	boundary := ((bits + unit - 1) / unit) * unit
	count := 1 << max(0, boundary-prefix.Bits())
	if count > 256 {
		return nil, ErrCapacity
	}
	base := prefix.Addr().As16()
	start, total := 0, 128
	if prefix.Addr().Is4() {
		start, total = 12, 32
	}
	var out []string
	for i := 0; i < count; i++ {
		addr := base
		for bit := 0; bit < boundary-prefix.Bits(); bit++ {
			if i&(1<<bit) != 0 {
				pos := boundary - 1 - bit
				addr[start+pos/8] |= 1 << (7 - pos%8)
			}
		}
		if total == 32 {
			parts := []string{}
			for j := boundary/8 - 1; j >= 0; j-- {
				parts = append(parts, strconv.Itoa(int(addr[12+j])))
			}
			out = append(out, strings.Join(parts, ".")+".in-addr.arpa.")
		} else {
			digits := hex.EncodeToString(addr[:])
			var name strings.Builder
			for j := boundary/4 - 1; j >= 0; j-- {
				name.WriteByte(digits[j])
				name.WriteByte('.')
			}
			name.WriteString("ip6.arpa.")
			out = append(out, name.String())
		}
	}
	return out, nil
}
func reverseZone(owner host.InstanceID, apex string, ttl uint32) *public.DNSZone {
	meta := metadata(owner, "unifi-rz-"+kinds.Digest([]byte(host.SourceID(owner) + apex))[:32])
	meta.Namespace = ""
	return &public.DNSZone{ObjectMeta: meta, Spec: public.DNSZoneSpec{ZoneName: apex, Type: public.ZoneTypePrimary, DefaultTTL: ttl, Policy: public.ZonePolicy{QueryAccess: public.AccessAllowOnlyPrivate, ZoneTransfer: public.AccessDeny, Notify: public.NotifyNone, Update: public.AccessDeny}}}
}
