package longterm

import (
	"testing"
	"time"
)

func TestNeeded(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		from time.Time
		want bool
	}{
		{"last hour", now.Add(-time.Hour), false},
		{"last day", now.Add(-24 * time.Hour), false},
		{"last day plus browser clock skew", now.Add(-24*time.Hour - 2*time.Minute), false},
		{"just over a day", now.Add(-25 * time.Hour), true},
		{"a week", now.Add(-7 * 24 * time.Hour), true},
		{"a year", now.AddDate(-1, 0, 0), true},
	} {
		if got := Needed(tc.from, now); got != tc.want {
			t.Errorf("%s: Needed = %v, want %v", tc.name, got, tc.want)
		}
	}
}
