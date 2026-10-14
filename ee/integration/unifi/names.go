package unifi

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	kinds "github.com/robertherbaugh/kudurru/api/integrationkinds/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

func objectName(name string) bool { return len(validation.IsDNS1123Subdomain(name)) == 0 }
func label(name string) bool      { return len(validation.IsDNS1123Label(name)) == 0 }

type formatPart struct {
	literal, field string
	defaults       []string
	slug           bool
}

func parseFormatter(raw string) ([]formatPart, error) {
	if len(raw) > 500 {
		return nil, ErrInvalid
	}
	var out []formatPart
	for raw != "" {
		start := strings.Index(raw, "{{")
		if start < 0 {
			if strings.Contains(raw, "}}") {
				return nil, ErrInvalid
			}
			out = append(out, formatPart{literal: raw})
			break
		}
		if start > 0 {
			out = append(out, formatPart{literal: raw[:start]})
		}
		raw = raw[start+2:]
		end := strings.Index(raw, "}}")
		if end < 0 {
			return nil, ErrInvalid
		}
		expression := strings.Split(raw[:end], "|")
		part := formatPart{field: strings.TrimSpace(expression[0])}
		if !field(part.field) {
			return nil, ErrInvalid
		}
		for _, operation := range expression[1:] {
			operation = strings.TrimSpace(operation)
			if operation == "slug" {
				if part.slug {
					return nil, ErrInvalid
				}
				part.slug = true
				continue
			}
			if part.slug || !strings.HasPrefix(operation, "default ") {
				return nil, ErrInvalid
			}
			value := strings.TrimSpace(strings.TrimPrefix(operation, "default "))
			if !field(value) {
				var text string
				if err := json.Unmarshal([]byte(value), &text); err != nil || len(text) > 253 {
					return nil, ErrInvalid
				}
			}
			part.defaults = append(part.defaults, value)
		}
		out = append(out, part)
		raw = raw[end+2:]
		if len(out) > 32 {
			return nil, ErrCapacity
		}
	}
	return out, nil
}
func field(raw string) bool {
	return slices.Contains([]string{".Alias", ".Name", ".Hostname", ".LocalDNSName", ".MAC", ".ID", ".Network.Domain"}, raw)
}
func value(raw string, d Device, network *Network) string {
	switch raw {
	case ".Alias":
		return d.Alias
	case ".Name":
		return d.Name
	case ".Hostname":
		return d.Hostname
	case ".LocalDNSName":
		return d.LocalDNSName
	case ".MAC":
		return d.MAC
	case ".ID":
		return d.ID
	case ".Network.Domain":
		if network != nil {
			return network.Domain
		}
		return ""
	default:
		var s string
		_ = json.Unmarshal([]byte(raw), &s)
		return s
	}
}
func slug(raw string) string {
	var out strings.Builder
	dash := false
	for _, c := range strings.ToLower(raw) {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			if dash && out.Len() > 0 {
				out.WriteByte('-')
			}
			dash = false
			out.WriteRune(c)
		} else {
			dash = true
		}
		if out.Len() >= 63 {
			break
		}
	}
	s := out.String()
	if len(s) > 63 {
		s = s[:63]
	}
	return strings.Trim(s, "-")
}
func baseName(raw string, d Device, network *Network) (string, error) {
	parts, err := parseFormatter(raw)
	if err != nil {
		return "", err
	}
	var result strings.Builder
	for _, part := range parts {
		if part.field == "" {
			result.WriteString(part.literal)
			continue
		}
		v := value(part.field, d, network)
		for _, fallback := range part.defaults {
			if v == "" {
				v = value(fallback, d, network)
			}
		}
		if part.slug {
			v = slug(v)
		}
		result.WriteString(v)
	}
	name := result.String()
	if len(parts) == 0 || name == "" {
		for _, candidate := range []string{d.Alias, d.LocalDNSName, d.Hostname, d.Name} {
			if name = slug(candidate); name != "" {
				break
			}
		}
		if name == "" {
			name = "client-" + kinds.Digest([]byte(d.ID))[:12]
		}
	}
	if !label(name) {
		return "", ErrInvalid
	}
	return name, nil
}

// NameRequests returns sorted stable IDs/base labels for root's durable allocator.
func NameRequests(in TranslationInput) ([]NameRequest, error) {
	prepared, _, err := prepare(in)
	if err != nil {
		return nil, err
	}
	return requests(prepared)
}
func requests(in TranslationInput) ([]NameRequest, error) {
	if in.Config.Inventory == nil {
		return nil, nil
	}
	var out []NameRequest
	for _, d := range in.Observation.Clients {
		if len(d.IPs) == 0 {
			continue
		}
		network := networkFor(d, in.Observation.Networks)
		if !included(in.Config.Inventory.IncludeNetworks, network) {
			continue
		}
		base, err := baseName(in.Config.Inventory.NameTemplate, d, network)
		if err != nil {
			return nil, err
		}
		out = append(out, NameRequest{ID: d.ID, Base: base})
	}
	slices.SortFunc(out, func(a, b NameRequest) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}
func names(requests []NameRequest, allocations map[string]string) (map[string]string, error) {
	if len(allocations) > maxClients {
		return nil, ErrCapacity
	}
	reserved := map[string]string{}
	for id, assigned := range allocations {
		if id == "" || len(id) > 256 || !label(assigned) || reserved[assigned] != "" {
			return nil, ErrAllocation
		}
		reserved[assigned] = id
	}
	counts := map[string]int{}
	for _, request := range requests {
		counts[request.Base]++
	}
	result := map[string]string{}
	for _, request := range requests {
		assigned, explicit := allocations[request.ID]
		if !explicit {
			assigned = request.Base
			if counts[assigned] > 1 || reserved[assigned] != "" && reserved[assigned] != request.ID {
				return nil, ErrAllocation
			}
		}
		if explicit && !allocatedLabel(request.Base, assigned) {
			return nil, ErrAllocation
		}
		if other := reserved[assigned]; other != "" && other != request.ID {
			return nil, ErrAllocation
		}
		reserved[assigned], result[request.ID] = request.ID, assigned
	}
	return result, nil
}
func allocatedLabel(base, assigned string) bool {
	if assigned == base {
		return true
	}
	i := strings.LastIndexByte(assigned, '-')
	if i < 0 {
		return false
	}
	n, err := strconv.ParseUint(assigned[i+1:], 10, 32)
	if err != nil || n < 2 || strconv.FormatUint(n, 10) != assigned[i+1:] {
		return false
	}
	suffix := assigned[i:]
	if len(suffix) >= 63 {
		return false
	}
	if len(base) > 63-len(suffix) {
		base = strings.TrimRight(base[:63-len(suffix)], "-")
	}
	return assigned == base+suffix
}
