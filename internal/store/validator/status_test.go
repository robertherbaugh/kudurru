package validator

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/robertherbaugh/kudurru/api/v1alpha1"
)

func readyCondition() metav1.Condition {
	return metav1.Condition{
		Type:               v1alpha1.ConditionReady,
		Status:             metav1.ConditionTrue,
		Reason:             v1alpha1.ReasonServing,
		Message:            "serving 42 records",
		ObservedGeneration: 3,
		LastTransitionTime: metav1.Date(2026, 10, 11, 0, 0, 0, 0, metav1.Now().Location()),
	}
}

func TestValidateIgnoresStatusOnMainResource(t *testing.T) {
	v := testValidator(t)
	z := zoneOf(typedSample(t, "DNSZone"))
	bad := readyCondition()
	bad.Reason = "" // invalid, but status is not part of a main-resource write
	z.Status.Conditions = []metav1.Condition{bad}
	if err := v.Validate(ctx(), z, nil); err != nil {
		t.Fatalf("create must ignore status: %v", err)
	}
	if len(z.Status.Conditions) != 1 {
		t.Fatal("Validate must not drop the caller's status; status handling belongs to the driver")
	}
	if err := v.Validate(ctx(), z, stored(typedSample(t, "DNSZone"))); err != nil {
		t.Fatalf("update must ignore status: %v", err)
	}
}

func TestValidateStatus(t *testing.T) {
	v := testValidator(t)
	old := stored(typedSample(t, "DNSZone"))
	cases := []struct {
		name   string
		mutate func(*v1alpha1.DNSZone)
		field  string
		typ    metav1.CauseType
	}{
		{"valid condition", func(z *v1alpha1.DNSZone) { z.Status.Conditions = []metav1.Condition{readyCondition()} }, "", ""},
		{"empty reason", func(z *v1alpha1.DNSZone) {
			c := readyCondition()
			c.Reason = ""
			z.Status.Conditions = []metav1.Condition{c}
		}, "status.conditions[0].reason", invalid},
		{"status enum", func(z *v1alpha1.DNSZone) {
			c := readyCondition()
			c.Status = "Maybe"
			z.Status.Conditions = []metav1.Condition{c}
		}, "status.conditions[0].status", notSupported},
		{"duplicate condition type (listType=map)", func(z *v1alpha1.DNSZone) {
			z.Status.Conditions = []metav1.Condition{readyCondition(), readyCondition()}
		}, "status.conditions[1]", duplicate},
		{"spec changes are ignored on a status write", func(z *v1alpha1.DNSZone) {
			z.Spec.ZoneName = "other.example."
			z.Spec.Type = "bogus"
			z.Status.Serial = 2026101101
		}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			z := zoneOf(typedSample(t, "DNSZone"))
			tc.mutate(z)
			err := v.ValidateStatus(ctx(), z, old)
			if tc.field == "" {
				if err != nil {
					t.Fatal(err)
				}
				return
			}
			wantInvalid(t, err, tc.field, tc.typ)
		})
	}
	if err := v.ValidateStatus(ctx(), typedSample(t, "DNSZone"), nil); err == nil {
		t.Fatal("ValidateStatus without the stored object must fail")
	}
}

// TestStatusRatcheting: a stored status that newer rules reject can be
// written back unchanged (the apiserver ratchets status too).
func TestStatusRatcheting(t *testing.T) {
	v := testValidator(t)
	c := readyCondition()
	c.Reason = ""
	old := zoneOf(stored(typedSample(t, "DNSZone")))
	old.Status.Conditions = []metav1.Condition{c}

	upd := zoneOf(typedSample(t, "DNSZone"))
	upd.Status.Conditions = []metav1.Condition{c}
	upd.Status.Serial = 7
	if err := v.ValidateStatus(ctx(), upd, old); err != nil {
		t.Fatalf("unchanged legacy condition must ratchet: %v", err)
	}
}
