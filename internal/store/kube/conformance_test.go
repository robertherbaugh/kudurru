package kube

import (
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

// TestConformance runs the driver conformance suite against a real
// kube-apiserver (envtest). Each case gets an empty control plane and a
// fresh driver.
func TestConformance(t *testing.T) {
	requireEnv(t)
	conformance.Run(t, func(t *testing.T) store.Store {
		purge(t, shared.admin)
		return newTestStore(t, Options{})
	})
}
