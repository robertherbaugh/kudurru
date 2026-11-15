package policy

// RangeDiagnoses visits immutable diagnoses in the same order as Diagnoses,
// without allocating a copy. Returning false stops iteration immediately.
// A nil index or callback has no diagnoses to visit.
func (ix *Index) RangeDiagnoses(yield func(Diagnosis) bool) {
	if ix == nil || yield == nil {
		return
	}
	for _, d := range ix.diags {
		if !yield(d) {
			return
		}
	}
}
