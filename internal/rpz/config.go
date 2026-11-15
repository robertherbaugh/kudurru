package rpz

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

// SourceIdentity binds runtime data to a list UID, source position, mode and
// entire source configuration. Shared status and secret values are excluded.
func SourceIdentity(list *v1alpha1.BlockList, index int) string {
	if list == nil || index < 0 || index >= len(list.Spec.Sources) || list.Spec.Sources[index].RPZ == nil {
		return ""
	}
	data, _ := json.Marshal(struct {
		UID, Namespace, Name string
		Index                int
		Mode                 v1alpha1.ListMode
		Source               v1alpha1.ListSource
	}{string(list.UID), list.Namespace, list.Name, index, list.Spec.Mode, list.Spec.Sources[index]})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// SourceName returns the explicit name or a stable source-position label.
func SourceName(list *v1alpha1.BlockList, index int) string {
	if list == nil || index < 0 || index >= len(list.Spec.Sources) {
		return ""
	}
	if name := list.Spec.Sources[index].Name; name != "" {
		return name
	}
	return fmt.Sprintf("rpz-%d", index)
}

func sourceConfig(list *v1alpha1.BlockList, index int) (Spec, *v1alpha1.DNSZone, error) {
	src := list.Spec.Sources[index]
	spec := Spec{List: list.Namespace + "/" + list.Name, Source: SourceName(list, index), Identity: SourceIdentity(list, index), Exclude: append([]string(nil), src.Exclude...)}
	if src.RPZ == nil {
		return spec, nil, fmt.Errorf("rpz: transfer configuration is required")
	}
	spec.Origin = src.RPZ.ZoneName
	if src.Format != "rpz" || src.Subdomains || list.Spec.Mode == "allow" ||
		src.URL != "" || src.Inline != nil || src.ConfigMapRef != nil || src.Entries != nil {
		return spec, nil, fmt.Errorf("rpz: source requires exclusive RPZ configuration, format rpz and block mode")
	}
	if _, err := Compile(spec, nil); err != nil {
		return spec, nil, err
	}
	if len(src.RPZ.Primaries) == 0 || len(src.RPZ.Primaries) > 32 {
		return spec, nil, fmt.Errorf("rpz: one to 32 primaries are required")
	}
	if protocol := src.RPZ.TransferProtocol; protocol != "" && protocol != "tcp" && protocol != "tls" {
		return spec, nil, fmt.Errorf("rpz: transfer protocol must be tcp or tls")
	}
	for _, primary := range src.RPZ.Primaries {
		if err := validatePrimary(primary); err != nil {
			return spec, nil, err
		}
	}
	if len(src.RPZ.TSIGKeyRef) > 253 || strings.ContainsAny(src.RPZ.TSIGKeyRef, " \t\r\n") {
		return spec, nil, fmt.Errorf("rpz: invalid TSIG key reference")
	}
	dz := &v1alpha1.DNSZone{Spec: v1alpha1.DNSZoneSpec{
		Type: v1alpha1.ZoneTypeSecondary, ZoneName: spec.Origin,
		Secondary: &v1alpha1.SecondarySpec{Primaries: append([]string(nil), src.RPZ.Primaries...),
			TransferProtocol: src.RPZ.TransferProtocol, TSIGKeyRef: src.RPZ.TSIGKeyRef},
	}}
	return spec, dz, nil
}

func validatePrimary(primary string) error {
	if primary == "" || len(primary) > 300 || strings.ContainsAny(primary, "/ \t\r\n") {
		return fmt.Errorf("rpz: primary must be a bounded host/IP and optional port")
	}
	host, port, err := net.SplitHostPort(primary)
	if err != nil {
		host, port = primary, "53"
		if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
			host = host[1 : len(host)-1]
		}
		if _, err := netip.ParseAddr(host); err != nil && strings.ContainsAny(host, ":[]") {
			return fmt.Errorf("rpz: invalid primary endpoint")
		}
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		if addr.Zone() != "" {
			return fmt.Errorf("rpz: scoped primary address is unsupported")
		}
	} else if _, ok := dns.IsDomainName(host); !ok || host == "." || strings.ContainsAny(host, ":[]\\*") {
		return fmt.Errorf("rpz: invalid primary hostname")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 || host == "" {
		return fmt.Errorf("rpz: invalid primary host or port")
	}
	return nil
}

func configured(list *v1alpha1.BlockList, index int) bool {
	return list != nil && !v1alpha1.IsDisabled(list) && list.Spec.Sources[index].IsEnabled() &&
		(list.Spec.Sources[index].RPZ != nil || list.Spec.Sources[index].Format == "rpz")
}
