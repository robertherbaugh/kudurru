package blocking

import (
	"testing"
	"time"
)

func TestToggleOnChange(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	tg := NewToggle(func() time.Time { return now })
	var calls []time.Time
	tg.SetOnChange(func(until time.Time) { calls = append(calls, until) })

	tg.Apply(time.Time{}) // already enabled: no change
	if len(calls) != 0 {
		t.Fatalf("Apply of the current value called the observer: %v", calls)
	}
	until, _ := tg.Disable(time.Minute)
	tg.Apply(until) // replicated echo of the same value
	if err := tg.Enable(); err != nil {
		t.Fatal(err)
	}
	tg.Apply(Forever)
	if len(calls) != 3 || !calls[0].Equal(until) || !calls[1].IsZero() || !calls[2].Equal(Forever) {
		t.Fatalf("observer calls %v, want [%v, zero, Forever]", calls, until)
	}
	tg.SetOnChange(nil)
	tg.Apply(time.Time{})
	if len(calls) != 3 {
		t.Fatal("observer called after removal")
	}
}
