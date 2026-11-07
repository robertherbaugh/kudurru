package sqlite

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

// TestConformance runs the shared Store suite with a fresh state.db in a
// temporary directory per case.
func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) store.Store { return openTemp(t) })
}

func TestRejectionConformance(t *testing.T) {
	conformance.RunRejections(t, func(t *testing.T) store.Store { return openTemp(t) }, func(t *testing.T, st store.Store, _ store.Key) any {
		s := st.(*Store)
		return struct {
			RV             string
			Changes, Audit int
		}{s.ResourceVersion(), countRows(t, s, "changes"), countRows(t, s, "audit")}
	})
}
