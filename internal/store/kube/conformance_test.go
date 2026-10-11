package kube

import (
	"os"
	"strings"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"github.com/robertherbaugh/kudurru/internal/store/conformance"
)

// knownImpossible lists conformance cases a real apiserver cannot satisfy,
// with the reason. The suite itself is unchanged; the factory skips these
// cases unless KUDURRU_CONFORMANCE_STRICT is set. Each entry has a
// driver-side test of the proposed replacement and a section in
// docs/spikes/store-kube.md with the exact suite change.
var knownImpossible = map[string]string{
	"ApplyOptimisticConcurrency": "re-applying an identical object is a no-op in the apiserver: " +
		"resourceVersion and generation do not change (CRD generation only moves on spec changes); " +
		"the proposed suite change is exercised by TestApplyOptimisticConcurrencyWithSpecChange",
}

// TestConformance runs the unchanged driver conformance suite against a
// real kube-apiserver (envtest). Each case gets an empty control plane and
// a fresh driver.
func TestConformance(t *testing.T) {
	requireEnv(t)
	strict := os.Getenv("KUDURRU_CONFORMANCE_STRICT") != ""
	conformance.Run(t, func(t *testing.T) store.Store {
		if !strict {
			name := t.Name()[strings.LastIndex(t.Name(), "/")+1:]
			if why, ok := knownImpossible[name]; ok {
				t.Skip("impossible against a real apiserver: " + why)
			}
		}
		purge(t, shared.admin)
		return newTestStore(t, Options{})
	})
}
