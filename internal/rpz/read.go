package rpz

import (
	"math"
	"time"
)

const maxSummarySources = 100000 // Same finite source budget as snapshot binding.

// Summary counts actual compiled policy owners, retaining diagnostic counts
// when any required source is missing, a placeholder, or expired. Availability
// uses the same absolute boundary as Evaluate. Nil selections and zero time
// deny; an explicit empty selection is available. Invalid bounds deny entirely.
// It performs no lookup, acquisition, mutation, or credential revalidation.
func (s *Selection) Summary(now time.Time) (rules int, available bool) {
	if s == nil || now.IsZero() || len(s.zones) > maxSummarySources {
		return 0, false
	}
	available = true
	for _, z := range s.zones {
		if z != nil {
			n := z.Stats().Rules
			if n < 0 || n > MaxRecords || rules > math.MaxInt-n {
				return 0, false
			}
			rules += n
		}
		available = z.AvailableAt(now) && available
	}
	return rules, available
}
