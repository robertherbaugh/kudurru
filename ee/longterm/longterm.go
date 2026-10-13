// Package longterm is the rule that makes long-term statistics a licensed
// feature (license.FeatureLongTerm, docs/architecture/licensing.md).
//
// The query log writer keeps writing rollups on every node whatever the
// license says, so a key that arrives later shows the history that was
// collected meanwhile; what the license gates is reading it: any statistics
// window that reaches back further than ShortWindow. The dashboard's live
// views (the last hours and the last day) stay free.
package longterm

import "time"

// ShortWindow is how far back statistics are free.
const ShortWindow = 24 * time.Hour

// slack absorbs the clock difference between a browser that computes "24
// hours ago" and the server that checks it.
const slack = 5 * time.Minute

// Needed reports whether a statistics window that starts at from, asked at
// now, is a long-term view that needs a license.
func Needed(from, now time.Time) bool {
	return now.Sub(from) > ShortWindow+slack
}
