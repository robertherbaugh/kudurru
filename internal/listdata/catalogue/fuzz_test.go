package catalogue

import (
	"testing"
)

// FuzzParse feeds arbitrary documents to the catalogue and AdGuard
// parsers (blocking.catalogue.url and adguardServicesURL are downloaded
// at runtime): neither may panic, a catalogue stays within its limits,
// and an import keeps only single-site ||name^ rules.
func FuzzParse(f *testing.F) {
	f.Add([]byte(adguardFixture))
	f.Add([]byte(`[{"id":"a_b","name":"A","group":"ai","rules":["||a.example^","/.*/","||com^"]}]`))
	f.Add([]byte(`{"blocked_services":[{"id":"x","name":"X","rules":["||x.example^$important","@@||y.example^"]}]}`))
	f.Fuzz(func(t *testing.T, b []byte) {
		if c, err := Parse(b, "fuzz"); err == nil {
			if len(c.Services) > maxServices || len(c.Categories) > maxCategories {
				t.Fatalf("catalogue over its limits: %d services, %d categories", len(c.Services), len(c.Categories))
			}
		}
		imp, err := ParseAdGuard(b, "fuzz")
		if err != nil {
			return
		}
		if len(imp.Services) == 0 || len(imp.Services) > maxServices {
			t.Fatalf("import with %d services", len(imp.Services))
		}
		for _, s := range imp.Services {
			if !idPattern.MatchString(s.ID) || len(s.Rules) == 0 || len(s.Rules) > maxRules {
				t.Fatalf("service %+v", s)
			}
			for _, r := range s.Rules {
				if !adguardRule(r) {
					t.Fatalf("service %s kept rule %q", s.ID, r)
				}
			}
		}
	})
}
