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

// An apply that removes the reserved finalizer names, for the audit trail,
// what ended the obligation; an apply that keeps or never had it names nothing.
func TestReservedFinalizerDropReason(t *testing.T) {
	const reserved = IntegrationCleanupFinalizer
	doc := func(policy string, finalizers ...string) *v1alpha1.Integration {
		return &v1alpha1.Integration{ObjectMeta: metav1.ObjectMeta{Finalizers: finalizers}, Spec: v1alpha1.IntegrationSpec{DeletionPolicy: policy}}
	}
	for _, tc := range []struct {
		name    string
		stored  *v1alpha1.Integration
		applied string // the document's policy before defaulting ("" is omitted)
		want    string
	}{
		{"live, explicit Retain", storedIntegration(false, reserved), "Retain", "disarmed: deletionPolicy is Retain; dropped " + reserved},
		{"live, omitted", storedIntegration(false, reserved), "", "disarmed: deletionPolicy is Retain; dropped " + reserved},
		{"deleting, explicit Retain", storedIntegration(true, reserved), "Retain", "released without cleanup: deletionPolicy is Retain; dropped " + reserved},
		{"deleting, omitted, another finalizer left", storedIntegration(true, reserved, "kudurru.io/operator-hold"), "", "released without cleanup: deletionPolicy is Retain; dropped " + reserved},
		{"live, stays Delete", storedIntegration(false, reserved), "Delete", ""},
		{"deleting, stays Delete", storedIntegration(true, reserved), "Delete", ""},
		{"never armed", storedIntegration(false), "Retain", ""},
		{"already cleared", storedIntegration(true, "kudurru.io/operator-hold"), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			applied := doc(tc.applied, reserved) // listing the name decides nothing
			next := applied.DeepCopy()
			if next.Spec.DeletionPolicy == "" {
				next.Spec.DeletionPolicy = v1alpha1.IntegrationDeletionRetain // the validator's default
			}
			ReconcileReservedFinalizers(next, tc.stored, true)
			if got := ReservedFinalizerDropReason(tc.stored, next); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
	// Only Integrations have a reserved finalizer, and a create drops nothing.
	zone := &v1alpha1.DNSZone{}
	held := &v1alpha1.DNSZone{ObjectMeta: metav1.ObjectMeta{Finalizers: []string{reserved}}}
	if got := ReservedFinalizerDropReason(held, zone); got != "" {
		t.Fatalf("another kind: %q", got)
	}
	if got := ReservedFinalizerDropReason(nil, doc("Retain")); got != "" {
		t.Fatalf("create: %q", got)
	}
}

// ReservedFinalizerUnarmed is true exactly for a stored object that an
// admitting driver would arm on its next apply.
func TestReservedFinalizerUnarmed(t *testing.T) {
	const reserved = IntegrationCleanupFinalizer
	with := func(i *v1alpha1.Integration, policy string) *v1alpha1.Integration {
		i.Spec.DeletionPolicy = policy
		return i
	}
	for name, tc := range map[string]struct {
		stored Object
		want   bool
	}{
		"live Delete without the finalizer":     {with(storedIntegration(false), "Delete"), true},
		"live Delete with another finalizer":    {with(storedIntegration(false, "kudurru.io/operator-hold"), "Delete"), true},
		"live Delete, armed":                    {with(storedIntegration(false, reserved), "Delete"), false},
		"live Retain":                           {with(storedIntegration(false), "Retain"), false},
		"live, no policy stored":                {storedIntegration(false), false},
		"deleting Delete, cleared by its owner": {with(storedIntegration(true, "kudurru.io/operator-hold"), "Delete"), false},
		"deleting Delete, armed":                {with(storedIntegration(true, reserved), "Delete"), false},
		"another kind":                          {&v1alpha1.DNSZone{}, false},
		"a nil Integration":                     {(*v1alpha1.Integration)(nil), false},
	} {
		if got := ReservedFinalizerUnarmed(tc.stored); got != tc.want {
			t.Fatalf("%s: unarmed=%v, want %v", name, got, tc.want)
		}
		// The predicate and the admission rule agree: an apply of the stored
		// spec changes the reserved finalizer exactly when it is true.
		if i, ok := tc.stored.(*v1alpha1.Integration); ok && i != nil {
			next := i.DeepCopy()
			ReconcileReservedFinalizers(next, i, true)
			if armed := !HasFinalizer(i, reserved) && HasFinalizer(next, reserved); armed != tc.want {
				t.Fatalf("%s: admission arms=%v, predicate=%v", name, armed, tc.want)
			}
		}
	}
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
