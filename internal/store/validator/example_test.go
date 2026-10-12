package validator_test

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/store/validator"
)

// Example shows the call a store driver makes before persisting, and how
// the API maps a rejection to its {error:{key,message,hint}} envelope.
func Example() {
	v, err := validator.New() // once per process; compiles the CEL rules
	if err != nil {
		panic(err)
	}
	zone := &v1alpha1.DNSZone{
		ObjectMeta: metav1.ObjectMeta{Name: "home"},
		Spec:       v1alpha1.DNSZoneSpec{ZoneName: "home.example.com", Type: v1alpha1.ZoneTypePrimary}, // no trailing dot
	}
	err = v.Validate(context.Background(), zone, nil)
	if ve, ok := validator.AsError(err); ok {
		fmt.Println(ve.HTTPStatus(), ve.Key())
		fmt.Println(ve.Hint())
		fmt.Println(ve.Causes[0].Field, ve.Causes[0].Type)
	}

	zone.Spec.ZoneName = "home.example.com."
	if err := v.Validate(context.Background(), zone, nil); err == nil {
		fmt.Println("accepted; defaultTTL =", zone.Spec.DefaultTTL)
	}
	// Output:
	// 400 invalid
	// fix the listed fields and retry: spec.zoneName
	// spec.zoneName FieldValueInvalid
	// accepted; defaultTTL = 300
}
