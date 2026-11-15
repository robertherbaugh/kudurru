package servingauth

import (
	"errors"
	"testing"
	"time"
)

func TestBrandingHeldViewOriginalClockBoundsAndReplacement(t *testing.T) {
	f := newFixture(t)
	from, until := f.now, f.now.Add(time.Hour)
	f.deps.License.Branding = true
	f.deps.License.BrandingFrom, f.deps.License.BrandingUntil = from, until
	old := f.view(t)
	defer old.Release()
	for _, tc := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"before-capture", from.Add(-time.Nanosecond), false},
		{"capture", from, true},
		{"before-grace-end", until.Add(-time.Nanosecond), true},
		{"exact-grace-end", until, false},
		{"after-grace-end", until.Add(time.Nanosecond), false},
		{"zero-clock", time.Time{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := old.BrandingAllowed(tc.now)
			if err != nil || got != tc.want {
				t.Fatalf("allowed=%v error=%v, want %v", got, err, tc.want)
			}
			allowed, deadline, err := old.BrandingValidity(tc.now)
			if err != nil || allowed != tc.want || tc.want && !deadline.Equal(until) || !tc.want && !deadline.IsZero() {
				t.Fatal("validity disagrees with decision or original absolute deadline", err)
			}
		})
	}
	// Same canonical image, different local license policy. Neither replacement
	// nor caller edits may change the original captured read's decision/deadline.
	f.deps.License = LicensePolicy{RBAC: true}
	denied := f.view(t)
	defer denied.Release()
	if got, err := denied.BrandingAllowed(from); err != nil || got {
		t.Fatal("replacement retained branding", err)
	}
	if got, err := old.BrandingAllowed(from); err != nil || !got {
		t.Fatal("replacement changed held original read", err)
	}
	if old.DependencyIdentity() == denied.DependencyIdentity() {
		t.Fatal("branding policy omitted from dependency identity")
	}
	f.deps.License = LicensePolicy{Branding: true, BrandingFrom: from, BrandingUntil: until.Add(time.Hour)}
	renewed := f.view(t)
	defer renewed.Release()
	if got, err := renewed.BrandingAllowed(until); err != nil || !got {
		t.Fatal("renewed view did not use its own deadline", err)
	}
	if got, err := old.BrandingAllowed(until); err != nil || got {
		t.Fatal("renewal extended original held deadline", err)
	}
	// Prove evaluation needs no live store, manager or lookup callback.
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := old.BrandingAllowed(from); err != nil || !got {
		t.Fatal("closed live store changed captured branding", err)
	}
	old.Release()
	if got, err := old.BrandingAllowed(from); got || !errors.Is(err, ErrInvalid) {
		t.Fatal("released view retained entitlement", err)
	}
	if got, err := (*View)(nil).BrandingAllowed(from); got || !errors.Is(err, ErrInvalid) {
		t.Fatal("nil view retained entitlement", err)
	}
}

func TestBrandingPrepareRejectsUnboundedGrant(t *testing.T) {
	f := newFixture(t)
	cut, err := CaptureImage(t.Context(), f.image(t), f.registry)
	if err != nil {
		t.Fatal(err)
	}
	for _, policy := range []LicensePolicy{
		{Branding: true},
		{Branding: true, BrandingFrom: f.now},
		{Branding: true, BrandingUntil: f.now.Add(time.Hour)},
		{Branding: true, BrandingFrom: f.now, BrandingUntil: f.now},
		{Branding: true, BrandingFrom: f.now, BrandingUntil: f.now.Add(-time.Second)},
		{Requests: true},
		{Requests: true, RequestsFrom: f.now},
		{Requests: true, RequestsUntil: f.now.Add(time.Hour)},
		{Requests: true, RequestsFrom: f.now, RequestsUntil: f.now},
	} {
		deps := f.deps
		deps.License = policy
		if _, err := Prepare(t.Context(), cut, deps); !errors.Is(err, ErrDependency) {
			t.Fatal("unbounded/invalid branding window prepared", err)
		}
	}
}

func TestCapturedUnblockRequestsOriginalPreviewWindow(t *testing.T) {
	f := newFixture(t)
	from, until := f.now, f.now.Add(time.Hour)
	f.deps.License = LicensePolicy{Requests: true, RequestsFrom: from, RequestsUntil: until}
	original := f.view(t)
	for _, tc := range []struct {
		now  time.Time
		want bool
	}{
		{from.Add(-time.Nanosecond), false},
		{from, true},
		{until.Add(-time.Nanosecond), true},
		{until, false},
		{until.Add(time.Second), false},
	} {
		if got, err := original.UnblockRequestsAllowed(tc.now); err != nil || got != tc.want {
			t.Fatal("request preview did not honor original clock window", err)
		}
	}
	f.deps.License = LicensePolicy{}
	denied := f.view(t)
	if got, err := denied.UnblockRequestsAllowed(from); err != nil || got {
		t.Fatal("replacement retained preview entitlement", err)
	}
	if original.DependencyIdentity() == denied.DependencyIdentity() {
		t.Fatal("preview entitlement omitted from dependency identity")
	}
	if err := f.st.Close(); err != nil {
		t.Fatal(err)
	}
	if got, err := original.UnblockRequestsAllowed(from); err != nil || !got {
		t.Fatal("replacement/live store altered original request preview", err)
	}
	original.Release()
	if got, err := original.UnblockRequestsAllowed(from); got || !errors.Is(err, ErrInvalid) {
		t.Fatal("released request preview retained entitlement", err)
	}
	if got, err := (*View)(nil).UnblockRequestsAllowed(from); got || !errors.Is(err, ErrInvalid) {
		t.Fatal("nil view retained preview entitlement", err)
	}
}
