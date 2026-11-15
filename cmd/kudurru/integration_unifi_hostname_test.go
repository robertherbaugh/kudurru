package main

import (
	"errors"
	"net/netip"
	"slices"
	"testing"
	"time"

	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	ih "github.com/robertherbaugh/kudurru/ee/integration/host"
	"github.com/robertherbaugh/kudurru/internal/store"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// D408: the zone driver writes a target zone's committed status after every
// change of its records, the previous observation's included. When that
// status write lands while the next acquisition is in flight, it changes only
// the zone's status and resource version, which nothing the observation is
// admitted against depends on (the instance's authority digest already
// excludes it), so the observation still commits and publishes. A change of
// the zone's spec still fences it.
func TestIntegrationUniFiObservationAdmitsTargetZoneStatusWrite(t *testing.T) {
	for _, change := range []string{"status", "spec"} {
		t.Run(change, func(t *testing.T) {
			controller := newUniFiHostnameController(t, "test-only-private-credential")
			s, path := integrationFixture(t, nil, nil)
			d := controller.definition(path)
			d.Spec.UniFi.Poll.Duration = time.Hour
			d = integrationApply(t, s, d)
			integrationRun(t, s)
			ip := netip.MustParseAddr("127.0.0.2")
			awaitServingRuntime(t, s, func() bool { return integrationInventory(t, s).Lookup(ip, time.Now()) != nil })
			owner := integrationOwner(s, d)
			integrationReadyCommand(t, s, owner)
			// Hold the next acquisition at its last provider response, after
			// its original cut was captured, and write the target zone there.
			// The read-only preflight above opened a fixture cut that no last
			// response closed; the held acquisition starts its own.
			controller.mu.Lock()
			controller.active = false
			controller.mu.Unlock()
			gate := controller.holdPhase(uniFiRenamed)
			defer gate.resume()
			type outcome struct {
				result *ih.Result
				err    error
			}
			done := make(chan outcome, 1)
			go func() {
				result, err := s.integrations.host.Sync(t.Context(), owner, "")
				done <- outcome{result, err}
			}()
			select {
			case <-gate.entered:
			case got := <-done:
				t.Fatalf("sync ended before the held response: %v", got.err)
			case <-time.After(15 * time.Second):
				t.Fatalf("acquisition did not reach the held response: attempts=%d", controller.calls.Load())
			}
			var z public.DNSZone
			if err := s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &z); err != nil {
				t.Fatal(err)
			}
			before := z.ResourceVersion
			if change == "status" {
				meta.SetStatusCondition(&z.Status.Conditions, metav1.Condition{Type: "D408Probe", Status: metav1.ConditionTrue, Reason: "Probe", Message: "status-only write"})
				if err := s.opts.Store.UpdateStatus(t.Context(), &z); err != nil {
					t.Fatal(err)
				}
			} else {
				z.Spec.Nameservers = append(z.Spec.Nameservers, "ns.other.test.")
				if _, err := s.opts.Store.Apply(t.Context(), &z, store.ApplyOptions{Manager: store.ManagerSystem, ExpectedResourceVersion: before}); err != nil {
					t.Fatal(err)
				}
			}
			var after public.DNSZone
			if err := s.opts.Store.Get(t.Context(), store.Key{Kind: "DNSZone", Name: "home"}, &after); err != nil || after.ResourceVersion == before {
				t.Fatal("target zone write did not advance its resource version", err)
			}
			gate.resume()
			var got outcome
			select {
			case got = <-done:
			case <-time.After(30 * time.Second):
				t.Fatal("held sync did not finish")
			}
			if change == "spec" {
				if !errors.Is(got.err, ih.ErrFenced) || got.result != nil && got.result.Source != nil && got.result.Source.Committed {
					t.Fatalf("an observation admitted against an older zone spec committed: %v", got.err)
				}
				return
			}
			if got.err != nil || got.result == nil || got.result.Source == nil || !got.result.Source.Committed || !got.result.Source.Published {
				t.Fatalf("a target zone status write fenced the observation: %v", got.err)
			}
		})
	}
}
