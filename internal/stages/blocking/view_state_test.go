package blocking

import (
	"testing"
	"time"

	"github.com/robertherbaugh/kudurru/internal/listdata/catalogue"
)

func TestViewStateAtAbsoluteDeadlineAndConfiguredPrecedence(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	until := now.Add(time.Minute)
	for _, tt := range []struct {
		name       string
		disabled   bool
		deadline   time.Time
		observed   time.Time
		wantEnable bool
	}{
		{"enabled", false, time.Time{}, now, true},
		{"finite-before", false, until, until.Add(-time.Nanosecond), false},
		{"finite-exact", false, until, until, true},
		{"finite-after", false, until, until.Add(time.Nanosecond), true},
		{"indefinite", false, Forever, now, false},
		{"beyond-forever", false, Forever.Add(time.Second), now, false},
		{"configured-off", true, time.Time{}, now, false},
		{"configured-off-before", true, until, now, false},
		{"configured-off-expired", true, until, until, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			v, err := NewView(Options{Disabled: tt.disabled}, tt.deadline, catalogue.Embedded())
			if err != nil {
				t.Fatal(err)
			}
			configured, enabled, raw, err := v.StateAt(tt.observed)
			if err != nil || configured == tt.disabled || enabled != tt.wantEnable || !raw.Equal(tt.deadline) {
				t.Fatal("incorrect captured blocking state", configured, enabled, raw, err)
			}
			// Observation cannot expire or rewrite the frozen absolute deadline.
			_, _, raw, err = v.StateAt(now)
			if err != nil || !raw.Equal(tt.deadline) {
				t.Fatal("state observation mutated the captured deadline", raw, err)
			}
		})
	}
}

func TestViewStateAtInvalidCaptureAndClock(t *testing.T) {
	for _, view := range []*View{nil, {}, {defaults: defaultsOf(Options{})}, {catalogue: catalogue.Embedded()}} {
		if _, _, _, err := view.StateAt(time.Now()); err == nil {
			t.Fatal("invalid view fabricated blocking state")
		}
	}
	v, err := NewView(Options{}, time.Time{}, catalogue.Embedded())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := v.StateAt(time.Time{}); err == nil {
		t.Fatal("zero observation time accepted")
	}
}
