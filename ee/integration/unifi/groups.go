package unifi

import (
	"slices"
	"strconv"
	"strings"
	"unicode"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func intString(n int) string { return strconv.Itoa(n) }

func groups(in TranslationInput) ([]*public.ClientGroup, int, error) {
	configured := map[string]map[string]bool{}
	for _, group := range in.Config.Groups.ByNetwork {
		configured[group] = map[string]bool{}
	}
	noMAC := 0
	for _, d := range in.Observation.Clients {
		if len(d.IPs) == 0 {
			continue
		}
		network := networkFor(d, in.Observation.Networks)
		if in.Config.Inventory != nil && !included(in.Config.Inventory.IncludeNetworks, network) {
			continue
		}
		membership := map[string]bool{}
		if network != nil {
			if group := in.Config.Groups.ByNetwork[network.Name]; group != "" {
				membership[group] = true
			}
		}
		if prefix := in.Config.Groups.ByNote; prefix != "" {
			for _, token := range strings.FieldsFunc(d.Note, func(r rune) bool { return unicode.IsSpace(r) || r == ',' || r == ';' }) {
				if !strings.HasPrefix(token, prefix) {
					continue
				}
				group := strings.TrimPrefix(token, prefix)
				if !objectName(group) {
					return nil, noMAC, ErrInvalid
				}
				membership[group] = true
			}
		}
		if d.MAC == "" {
			if len(membership) > 0 {
				noMAC++
			}
			continue
		}
		for group := range membership {
			if configured[group] == nil {
				configured[group] = map[string]bool{}
			}
			configured[group][d.MAC] = true
		}
		if len(configured) > 256 {
			return nil, noMAC, ErrCapacity
		}
	}
	var out []*public.ClientGroup
	for name, members := range configured {
		if len(members) > 1000 {
			return nil, noMAC, ErrCapacity
		}
		group := &public.ClientGroup{ObjectMeta: metadata(in.Owner, name), Spec: public.ClientGroupSpec{Sources: &public.ClientGroupSources{IntegrationRef: &public.ObjectRef{Name: in.Owner.Name}}}}
		for mac := range members {
			group.Spec.Clients = append(group.Spec.Clients, public.ClientMatcher{MAC: mac})
		}
		slices.SortFunc(group.Spec.Clients, func(a, b public.ClientMatcher) int { return strings.Compare(a.MAC, b.MAC) })
		out = append(out, group)
	}
	slices.SortFunc(out, func(a, b *public.ClientGroup) int { return strings.Compare(a.Name, b.Name) })
	return out, noMAC, nil
}
