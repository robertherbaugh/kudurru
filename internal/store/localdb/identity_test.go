package localdb

import "testing"

func TestCheckClusterIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, expected, observed, saved string
		admit                           bool
	}{
		{"offline matching", "expected", "", "expected", true},
		{"online matching", "expected", "expected", "expected", true},
		{"missing pin", "", "expected", "expected", false},
		{"copied volume", "expected", "", "other", false},
		{"online mismatch", "expected", "other", "expected", false},
		{"legacy snapshot", "expected", "expected", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := CheckClusterIdentity(tc.expected, tc.observed, tc.saved)
			if (err == nil) != tc.admit {
				t.Fatalf("saved identity admission: accepted=%t want=%t", err == nil, tc.admit)
			}
		})
	}
}
