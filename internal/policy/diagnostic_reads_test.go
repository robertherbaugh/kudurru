package policy

import (
	"slices"
	"testing"
)

func TestRangeDiagnosesPreservesOrderStopsAndDoesNotExposeMutableState(t *testing.T) {
	ix := &Index{diags: []Diagnosis{{Kind: "Policy", Object: "apps/a", Reason: "MissingReference", Message: "first"}, {Kind: "Policy", Object: "apps/b", Reason: "Disabled", Message: "second"}}}
	var got []Diagnosis
	ix.RangeDiagnoses(func(d Diagnosis) bool { got = append(got, d); return true })
	if !slices.Equal(got, ix.Diagnoses()) {
		t.Fatal("iteration changed diagnosis ordering", got)
	}
	got[0].Message = "mutated"
	calls := 0
	ix.RangeDiagnoses(func(d Diagnosis) bool {
		calls++
		if d.Message != "first" {
			t.Fatal("iterator exposed mutable diagnosis", d)
		}
		return false
	})
	if calls != 1 {
		t.Fatal("iteration continued after stop", calls)
	}
	ix.RangeDiagnoses(nil)
	var missing *Index
	missing.RangeDiagnoses(func(Diagnosis) bool { t.Fatal("nil index emitted a diagnosis"); return true })
}
