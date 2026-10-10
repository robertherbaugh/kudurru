package pipeline

// Status is the ≤32-value taxonomy shared by the query log, metrics and UI
// colours (docs/architecture.md §11.2). Values ≥ 16 were not answered
// normally.
type Status uint8

// Answered-normally statuses (< 16).
const (
	StatusUnknown Status = iota
	StatusForwarded
	StatusCached
	StatusCachedStale
	StatusAuthoritative
	StatusLocalRecord
	StatusPrefetchRefresh
	StatusSpecialDomain
	StatusRetried
	StatusInProgress
)

// Blocked and error statuses (>= 16).
const (
	StatusBlockedList Status = 16 + iota
	StatusBlockedRegex
	StatusBlockedExact
	StatusBlockedCNAME
	StatusBlockedUpstream
	StatusBlockedRPZ // reserved
	StatusBlockedSafeSearch
	StatusBlockedRateLimit
	StatusRefusedACL
	StatusDropped
	StatusServfailUpstream
	StatusTimeoutUpstream
	StatusDNSSECBogus
)

var statusNames = map[Status]string{
	StatusUnknown:           "unknown",
	StatusForwarded:         "forwarded",
	StatusCached:            "cached",
	StatusCachedStale:       "cached_stale",
	StatusAuthoritative:     "authoritative",
	StatusLocalRecord:       "local_record",
	StatusPrefetchRefresh:   "prefetch_refresh",
	StatusSpecialDomain:     "special_domain",
	StatusRetried:           "retried",
	StatusInProgress:        "in_progress",
	StatusBlockedList:       "blocked_list",
	StatusBlockedRegex:      "blocked_regex",
	StatusBlockedExact:      "blocked_exact",
	StatusBlockedCNAME:      "blocked_cname",
	StatusBlockedUpstream:   "blocked_upstream",
	StatusBlockedRPZ:        "blocked_rpz",
	StatusBlockedSafeSearch: "blocked_safesearch",
	StatusBlockedRateLimit:  "blocked_rate_limit",
	StatusRefusedACL:        "refused_acl",
	StatusDropped:           "dropped",
	StatusServfailUpstream:  "servfail_upstream",
	StatusTimeoutUpstream:   "timeout_upstream",
	StatusDNSSECBogus:       "dnssec_bogus",
}

// String returns the symbolic name used by the API and metrics.
func (s Status) String() string {
	if n, ok := statusNames[s]; ok {
		return n
	}
	return "unknown"
}

// Class is the coarse grouping the UI colours by.
type Class string

// Classes.
const (
	ClassPermitted Class = "permitted"
	ClassCached    Class = "cached"
	ClassBlocked   Class = "blocked"
	ClassError     Class = "error"
)

// Class maps a status to its class.
func (s Status) Class() Class {
	switch {
	case s == StatusCached || s == StatusCachedStale:
		return ClassCached
	case s >= StatusBlockedList && s <= StatusBlockedRateLimit:
		return ClassBlocked
	case s >= StatusRefusedACL:
		return ClassError
	default:
		return ClassPermitted
	}
}
