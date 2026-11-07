package main

import (
	"net/netip"
	"slices"
	"testing"
	"time"

	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
)

// Explicit real-host syncs distinguish an accepted complete removal from a
// transient acquisition failure that merely ages out previous identity.
func TestIntegrationUniFiHostnameConflictAndOmissionCommit(t *testing.T) {
	controller := newUniFiHostnameController(t, "test-only-private-credential")
	s, path := integrationFixture(t, nil, nil)
	d := controller.definition(path)
	d.Spec.UniFi.Poll.Duration = time.Hour
	d = integrationApply(t, s, d)
	integrationRun(t, s)
	t.Cleanup(func() {
		if t.Failed() {
			for _, status := range s.integrations.host.Statuses() {
				t.Log("source condition", status.Phase, status.Reason)
			}
			t.Log("controller attempts", controller.calls.Load())
		}
	})
	ip := netip.MustParseAddr("127.0.0.2")
	awaitServingRuntime(t, s, func() bool {
		return integrationInventory(t, s).Lookup(ip, time.Now()) != nil
	})
	before := integrationInventory(t, s)
	client := before.Lookup(ip, time.Now())
	if client == nil || !slices.Contains(client.Names(), "phone.unifi.lan") {
		t.Fatal("actual legacy HTTP hostname did not reach complete inventory")
	}
	if before.Lookup(ip, client.ExpiresAt) != nil || before.Lookup(ip, client.ExpiresAt.Add(-time.Nanosecond)) == nil {
		t.Fatal("original source assignment deadline was not exclusive")
	}
	owner := integrationOwner(s, d)
	for _, phase := range []int32{uniFiConflict, uniFiNames, uniFiOmitted, uniFiNames} {
		controller.phase.Store(phase)
		integrationReadyCommand(t, s, owner)
		result, err := s.integrations.host.Sync(t.Context(), owner, "")
		if err != nil || result == nil || result.Source == nil || !result.Source.Committed || !result.Source.Published {
			t.Fatalf("complete hostname phase=%d was not acknowledged: %v", phase, err)
		}
		awaitServingRuntime(t, s, func() bool {
			fresh := integrationInventory(t, s)
			return (phase == uniFiNames) == (fresh.Lookup(ip, time.Now()) != nil)
		})
		if before.Lookup(ip, client.ObservedAt) == nil || before.Lookup(ip, client.ExpiresAt) != nil {
			t.Fatal("a successor publication changed the original owner/deadline")
		}
	}
	if client.Source != ih.SourceID(owner) {
		t.Fatal("hostname detached from its original integration owner")
	}
	controller.phase.Store(uniFiRenamed)
	if err := s.opts.Store.Delete(t.Context(), store.KeyOf(d), store.DeleteOptions{Manager: store.ManagerAPI}); err != nil {
		t.Fatal(err)
	}
	s.integrations.host.Wake()
	awaitServingRuntime(t, s, func() bool { return integrationInventory(t, s).Lookup(ip, time.Now()) == nil })
	recreated := controller.definition(path)
	recreated.Spec.UniFi.Poll.Duration = time.Hour
	recreated = integrationApply(t, s, recreated)
	if recreated.UID == d.UID {
		t.Fatal("same-name source recreation retained its old UID")
	}
	awaitServingRuntime(t, s, func() bool {
		fresh := integrationInventory(t, s).Lookup(ip, time.Now())
		return fresh != nil && fresh.Source == ih.SourceID(integrationOwner(s, recreated)) && slices.Contains(fresh.Names(), "safe.other.lan")
	})
	if before.Lookup(ip, client.ObservedAt).Source != client.Source || before.Lookup(ip, client.ExpiresAt) != nil {
		t.Fatal("new source UID rewrote the held original owner")
	}
}
