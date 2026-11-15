package validator

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/robertherbaugh/kudurru/internal/store"
	"k8s.io/apimachinery/pkg/api/validate"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestStoredValidationMatchesOriginalAdmissionAndPreservesObjects(t *testing.T) {
	v := &Validator{kinds: testValidator(t).kinds}
	check := func(t *testing.T, obj store.Object) {
		t.Helper()
		before, err := json.Marshal(obj)
		if err != nil {
			t.Fatal(err)
		}
		want := v.Validate(t.Context(), obj.DeepCopyObject().(store.Object), nil)
		if want == nil && v.HasStatusSubresource(store.KindOf(obj)) {
			want = v.ValidateStatus(t.Context(), obj.DeepCopyObject().(store.Object), obj.DeepCopyObject().(store.Object))
		}
		for range 2 {
			got := v.ValidateStored(t.Context(), obj)
			if (want == nil) != (got == nil) {
				t.Fatal("stored and ordinary validation disagree", want, got)
			}
			if want != nil {
				var wantFields, gotFields *Error
				if !errors.As(want, &wantFields) || !errors.As(got, &gotFields) || len(wantFields.Causes) != len(gotFields.Causes) {
					t.Fatal("stored validation lost rejection details", want, got)
				}
			}
		}
		after, err := json.Marshal(obj)
		if err != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("stored validation defaulted or changed the source", err)
		}
	}
	for kind := range sampleFiles {
		t.Run(string(kind), func(t *testing.T) { check(t, typedSample(t, kind)) })
	}
	t.Run("stored status retains original ratcheting", func(t *testing.T) {
		obj := typedSample(t, "DNSZone")
		check(t, obj)
		zoneOf(obj).Status.Conditions = []metav1.Condition{{Type: "Ready", Status: "invalid", Reason: "Original", LastTransitionTime: metav1.Now()}}
		check(t, obj)
	})
	for _, tc := range invalidCases() {
		t.Run(tc.name, func(t *testing.T) {
			obj := typedSample(t, tc.kind)
			// First retain the valid form, then change the exact encoded input.
			check(t, obj)
			tc.mutate(obj)
			check(t, obj)
		})
	}
}

func TestStoredValidationCacheBoundsCancellationAndContextMode(t *testing.T) {
	v := &Validator{kinds: testValidator(t).kinds}
	obj := typedSample(t, "DNSZone")
	if err := v.ValidateStored(t.Context(), obj); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := v.ValidateStored(ctx, obj); !errors.Is(err, context.Canceled) {
		t.Fatal("cached success ignored cancellation", err)
	}
	if err := v.ValidateStored(validate.WithAllDeclarativeEnforcedForTest(t.Context()), obj); err != nil {
		t.Fatal(err)
	}
	if len(v.stored.keys) != 2 {
		t.Fatal("context validation modes shared a success result")
	}
	for i := range storedValidationCapacity + 10 {
		next := obj.DeepCopyObject().(store.Object)
		next.SetName(fmt.Sprintf("stored-%d", i))
		if err := v.ValidateStored(t.Context(), next); err != nil {
			t.Fatal(err)
		}
	}
	if len(v.stored.keys) != storedValidationCapacity || len(v.stored.order) != storedValidationCapacity {
		t.Fatal("stored validation exceeded its finite retention bound")
	}
	if err := v.ValidateStored(t.Context(), obj); err != nil {
		t.Fatal("evicted input no longer validates", err)
	}
	if err := v.ValidateStored(t.Context(), nil); err == nil {
		t.Fatal("nil object accepted")
	}
}

func TestStoredValidationConcurrentExactInputs(t *testing.T) {
	v := &Validator{kinds: testValidator(t).kinds}
	obj := typedSample(t, "DNSZone")
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 16 {
				if err := v.ValidateStored(t.Context(), obj); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
	if len(v.stored.keys) != 1 {
		t.Fatal("identical validated inputs consumed multiple entries")
	}
}
