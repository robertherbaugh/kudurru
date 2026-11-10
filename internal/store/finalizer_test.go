package store

import (
	"errors"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func storedIntegration(deleting bool, finalizers ...string) *v1alpha1.Integration {
	i := &v1alpha1.Integration{ObjectMeta: metav1.ObjectMeta{Name: "cleanup", Namespace: "apps", UID: "uid-1", ResourceVersion: "7", Finalizers: finalizers}}
	if deleting {
		at := metav1.NewTime(time.Unix(100, 0))
		i.DeletionTimestamp = &at
	}
	return i
}

// The rule of D367 as a table: what an Apply stores for the reserved
// finalizer, by policy, stored state and whether the driver admits writes.
func TestIntegrationCleanupArmedRule(t *testing.T) {
	const reserved = IntegrationCleanupFinalizer
	for _, tc := range []struct {
		name         string
		policy       string
		stored       *v1alpha1.Integration // nil: create
		sqlite, kube bool
	}{
		{"create with Delete", "Delete", nil, true, false},
		{"create with Retain", "Retain", nil, false, false},
		{"create with the default", "", nil, false, false},
		{"unknown policy is never armed", "Erase", storedIntegration(false, reserved), false, false},
		{"live, armed, stays Delete", "Delete", storedIntegration(false, reserved), true, true},
		{"live, unarmed, becomes Delete", "Delete", storedIntegration(false), true, false},
		{"live, armed, becomes Retain", "Retain", storedIntegration(false, reserved), false, false},
		{"deleting, armed, stays Delete", "Delete", storedIntegration(true, reserved), true, true},
		{"deleting, armed, becomes Retain", "Retain", storedIntegration(true, reserved), false, false},
		{"deleting, already cleared, stays Delete", "Delete", storedIntegration(true, "kudurru.io/operator-hold"), false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for admits, want := range map[bool]bool{true: tc.sqlite, false: tc.kube} {
				var stored metav1.Object
				if tc.stored != nil {
					stored = tc.stored
				}
				if got := IntegrationCleanupArmed(tc.policy, stored, admits); got != want {
					t.Fatalf("admits=%v: armed=%v, want %v", admits, got, want)
				}
				// The caller's document decides nothing, whether it lists the
				// reserved name or not, and keeps its own finalizers in order.
				for _, claimed := range [][]string{nil, {reserved}, {reserved, "kudurru.io/operator-hold"}, {"kudurru.io/operator-hold", reserved, "example.com/other"}} {
					next := &v1alpha1.Integration{ObjectMeta: metav1.ObjectMeta{Finalizers: claimed}, Spec: v1alpha1.IntegrationSpec{DeletionPolicy: tc.policy}}
					before := append([]string(nil), claimed...)
					ReconcileReservedFinalizers(next, stored, admits)
					var expect []string
					for _, name := range before {
						if name != reserved {
							expect = append(expect, name)
						}
					}
					if want {
						expect = append(expect, reserved)
					}
					if !reflect.DeepEqual(next.Finalizers, expect) {
						t.Fatalf("admits=%v claimed=%v: finalizers=%v, want %v", admits, before, next.Finalizers, expect)
					}
				}
			}
		})
	}
	// Only Integrations have a reserved finalizer.
	zone := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{reserved}}}
	ReconcileReservedFinalizers(zone, nil, true)
	if !reflect.DeepEqual(zone.Finalizers, []string{reserved}) {
		t.Fatalf("another kind's finalizers were rewritten: %v", zone.Finalizers)
	}
	var absent *v1alpha1.Integration
	ReconcileReservedFinalizers(absent, nil, true) // must not panic
}

func TestPlanFinalizerExactIdentityAndTermination(t *testing.T) {
	const reserved = IntegrationCleanupFinalizer
	exact := func(name string, remove bool) FinalizerOptions {
		return FinalizerOptions{Finalizer: name, Remove: remove, ExpectedUID: "uid-1", ExpectedResourceVersion: "7"}
	}
	for _, tc := range []struct {
		name    string
		stored  *v1alpha1.Integration
		opts    FinalizerOptions
		want    []string
		changed bool
		err     error
	}{
		{"add to none", storedIntegration(false), exact(reserved, false), []string{reserved}, true, nil},
		{"add keeps order", storedIntegration(false, "example.com/a"), exact(reserved, false), []string{"example.com/a", reserved}, true, nil},
		{"add when present", storedIntegration(false, reserved), exact(reserved, false), []string{reserved}, false, nil},
		{"remove one of two", storedIntegration(false, "example.com/a", reserved), exact(reserved, true), []string{"example.com/a"}, true, nil},
		{"remove the last", storedIntegration(true, reserved), exact(reserved, true), nil, true, nil},
		{"remove when absent", storedIntegration(true, "example.com/a"), exact(reserved, true), []string{"example.com/a"}, false, nil},
		{"add while deleting", storedIntegration(true, "example.com/a"), exact(reserved, false), nil, false, ErrTerminating},
		{"present while deleting is no change", storedIntegration(true, reserved), exact(reserved, false), []string{reserved}, false, nil},
		{"stale resourceVersion", storedIntegration(false, reserved), FinalizerOptions{Finalizer: reserved, Remove: true, ExpectedUID: "uid-1", ExpectedResourceVersion: "6"}, nil, false, ErrConflict},
		{"another incarnation", storedIntegration(false, reserved), FinalizerOptions{Finalizer: reserved, Remove: true, ExpectedUID: "uid-2", ExpectedResourceVersion: "7"}, nil, false, ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := append([]string(nil), tc.stored.Finalizers...)
			got, changed, err := PlanFinalizer(tc.stored, tc.opts)
			if !errors.Is(err, tc.err) || changed != tc.changed || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %v changed=%v err=%v; want %v changed=%v err=%v", got, changed, err, tc.want, tc.changed, tc.err)
			}
			if !reflect.DeepEqual(tc.stored.Finalizers, before) {
				t.Fatal("the stored object was modified")
			}
		})
	}
	for name, opts := range map[string]FinalizerOptions{
		"no UID":             {Finalizer: reserved, ExpectedResourceVersion: "7"},
		"no resourceVersion": {Finalizer: reserved, ExpectedUID: "uid-1"},
		"empty name":         {ExpectedUID: "uid-1", ExpectedResourceVersion: "7"},
		"unqualified name":   {Finalizer: "cleanup", ExpectedUID: "uid-1", ExpectedResourceVersion: "7"},
		"invalid name":       {Finalizer: "kudurru.io/has space", ExpectedUID: "uid-1", ExpectedResourceVersion: "7"},
	} {
		if err := opts.Validate(); err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if _, _, err := PlanFinalizer(storedIntegration(false, reserved), opts); err == nil || errors.Is(err, ErrConflict) {
			t.Fatalf("%s: got %v, want a validation error", name, err)
		}
	}
	if !Terminated(storedIntegration(true)) || Terminated(storedIntegration(true, reserved)) || Terminated(storedIntegration(false)) {
		t.Fatal("Terminated must mean being deleted with no finalizer left")
	}
}
