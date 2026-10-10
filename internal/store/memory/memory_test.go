package memory

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

func TestConformance(t *testing.T) {
	conformance.Run(t, func(t *testing.T) store.Store {
		s := New()
		t.Cleanup(func() { _ = s.Close() })
		return s
	})
}
