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

// CacheUntil exposes the original absolute source boundary, without renewing
// an unavailable/expired copy. Client-IP rules cannot qualify a shared cache
// whose key has no original client identity. An empty selection has no boundary.
func (s *Selection) CacheUntil(now time.Time) (until time.Time, available bool) {
	if s == nil || now.IsZero() || len(s.zones) > maxSummarySources {
		return time.Time{}, false
	}
	for _, z := range s.zones {
		if !z.AvailableAt(now) || len(z.clients) != 0 {
			return time.Time{}, false
		}
		if until.IsZero() || z.spec.ExpiresAt.Before(until) {
			until = z.spec.ExpiresAt
		}
	}
	return until, true
}
