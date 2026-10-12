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
