package v1alpha1

import (
	"slices"
	"strings"
)

// EffectiveClientGroupRefs returns a detached copy of the list's namespace-local
// scope. Reserved UI names retain their original scope even for legacy objects.
// Admission rejects explicit contradictory scopes; this also prevents older
// stored objects or unvalidated inputs from widening those names.
func (b *BlockList) EffectiveClientGroupRefs() []string {
	if b.Name == "ui-allow" || b.Name == "ui-deny" {
		return nil
	}
	for _, prefix := range []string{"ui-allow-", "ui-deny-"} {
		if name, ok := strings.CutPrefix(b.Name, prefix); ok && name != "" {
			return []string{name}
		}
	}
	return slices.Clone(b.Spec.ClientGroupRefs)
}
