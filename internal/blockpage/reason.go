package blockpage

import (
	"context"
	"net/netip"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

// Reason kinds.
const (
	KindList       = "list"
	KindService    = "service"
	KindSafeSearch = "safesearch"
	KindPolicy     = "policy"
	KindCNAME      = "cname"
)

// List-name prefixes of the blocked-services and safe-search pseudo lists
// (internal/stages/blocking).
const (
	servicePrefix    = "service:"
	safeSearchPrefix = "safesearch:"
)

// Reason says why a name was blocked.
type Reason struct {
	// Kind is list, service, safesearch, policy (allow-only: no allow list
	// matched) or cname (an answer pointed to a blocked name or address).
	Kind string
	// List is the deciding list as the query log names it ("ns/name",
	// "service:<id>", "safesearch:<engine>"); empty for allow-only.
	List string
	// Rule is the matching rule text; empty when unknown or hidden.
	Rule string
	// Policy is the policy that applies to the client, "ns/name".
	Policy string
	// Status is the query-log status (blocked_list, ...).
	Status string
	// Time is when the query was answered; zero when the reason was
	// evaluated rather than looked up.
	Time time.Time
}

// Summary is the one-line explanation shown on the page.
func (r *Reason) Summary() string {
	switch r.Kind {
	case KindService:
		return "The name belongs to a blocked service (" + strings.TrimPrefix(r.List, servicePrefix) + ")."
	case KindSafeSearch:
		return "Safe search is enforced for this search engine (" + strings.TrimPrefix(r.List, safeSearchPrefix) + ")."
	case KindPolicy:
		return "Only names on an allow list may be resolved on this device, and this one is not on one."
	case KindCNAME:
		return "The name points to another name or address on a block list."
	}
	return "The name is on a block list."
}

// LabelValue is one labelled detail row.
type LabelValue struct{ Label, Value string }

// ListLabel is the detail row naming the deciding list, nil when none.
func (r *Reason) ListLabel() *LabelValue {
	switch {
	case r.List == "":
		return nil
	case r.Kind == KindService:
		return &LabelValue{"Service", strings.TrimPrefix(r.List, servicePrefix)}
	case r.Kind == KindSafeSearch:
		return &LabelValue{"Search engine", strings.TrimPrefix(r.List, safeSearchPrefix)}
	}
	return &LabelValue{"List", r.List}
}

// reasonOf classifies a query-log status and list.
func reasonOf(status pipeline.Status, ede uint16, hasEDE bool, list, rule string) Reason {
	r := Reason{List: list, Rule: rule, Status: status.String()}
	switch {
	case status == pipeline.StatusBlockedSafeSearch || strings.HasPrefix(list, safeSearchPrefix):
		r.Kind = KindSafeSearch
	case strings.HasPrefix(list, servicePrefix):
		r.Kind = KindService
	case status == pipeline.StatusBlockedCNAME:
		r.Kind = KindCNAME
	case list == "" || (hasEDE && ede == 17): // EDE 17 Filtered: allow-only
		r.Kind = KindPolicy
	default:
		r.Kind = KindList
	}
	return r
}

// pageStatuses are the statuses of the blocking stage's own blocks, the
// only ones whose answer can bring a browser to the block page (not
// blocked_upstream, rate limits).
var pageStatuses = []pipeline.Status{
	pipeline.StatusBlockedList, pipeline.StatusBlockedRegex, pipeline.StatusBlockedExact,
	pipeline.StatusBlockedCNAME, pipeline.StatusBlockedSafeSearch,
}

func pageStatus(name string) (pipeline.Status, bool) {
	for _, s := range pageStatuses {
		if s.String() == name {
			return s, true
		}
	}
	return pipeline.StatusUnknown, false
}

// Window is how far back the query log is searched for the query that
// brought the client here.
const Window = 10 * time.Minute

// Finder looks up the reason a client's query for domain (lower case, no
// trailing dot) was blocked. ok false with a note explains why there is
// none.
type Finder interface {
	Find(ctx context.Context, client netip.Addr, domain string, now time.Time) (r Reason, ok bool, note string)
}

// tailScan bounds how many of the newest live-tail events are scanned
// before the query log database is asked.
const tailScan = 8192

// QuerylogFinder finds the reason in the query log: the live tail (the
// in-memory ring, which has the events the database has not flushed yet)
// and then querylog.db, newest first within Window. The privacy mode
// decides what can be matched: under full the client address and the
// name; under anonymize_client the name and the client's /24 or /48
// (prefix) or its pseudonym; hide_domains and stricter modes keep no name
// to match, so no reason is shown.
type QuerylogFinder struct {
	// Tail is the live tail; nil skips it.
	Tail *querylog.Tail
	// Reader is the querylog.db reader; nil skips it (sink slog).
	Reader *sqlitelog.Reader
	// Privacy returns the current querylog.privacy and querylog.anonymize
	// settings; nil means full.
	Privacy func() (mode querylog.Mode, pseudonym bool)
	// Pseudonym returns the client name a client's queries are logged
	// under with pseudonyms (querylog.Async.Pseudonym); nil or "" matches
	// no row, so the page never shows another device's block.
	Pseudonym func(client netip.Addr) string
	// Disabled is true when querylog.enabled is false.
	Disabled bool
}

// Notes shown instead of a reason.
const (
	noteDisabled  = "The query log is off on this server, so the reason cannot be shown."
	notePrivacy   = "The query log of this server does not record domain names, so the reason cannot be shown."
	noteNotFound  = "No query for this name from your device was found in the recent query log, so the reason cannot be shown."
	noteNoDomain  = "The request did not name a domain."
	noteNoFinding = "The reason cannot be shown."
)

// Find implements Finder.
func (f *QuerylogFinder) Find(ctx context.Context, client netip.Addr, domain string, now time.Time) (Reason, bool, string) {
	if f.Disabled {
		return Reason{}, false, noteDisabled
	}
	mode, pseudonym := querylog.PrivacyFull, false
	if f.Privacy != nil {
		mode, pseudonym = f.Privacy()
	}
	match, name := client.Unmap(), ""
	switch mode {
	case querylog.PrivacyFull:
	case querylog.PrivacyAnonymizeClient:
		if pseudonym {
			if f.Pseudonym != nil && match.IsValid() {
				name = f.Pseudonym(match)
			}
			if name == "" {
				return Reason{}, false, noteNotFound
			}
			match = netip.Addr{}
		} else if match.IsValid() {
			bits := 24
			if match.Is6() {
				bits = 48
			}
			match = netip.PrefixFrom(match, bits).Masked().Addr()
		}
	default:
		return Reason{}, false, notePrivacy
	}
	since := now.Add(-Window)
	if f.Tail != nil {
		if r, ok := f.scanTail(match, name, domain, since); ok {
			return r, true, ""
		}
	}
	if f.Reader != nil {
		filter := sqlitelog.QueryFilter{Domain: domain, Status: pageStatuses, Since: since, Limit: 1, Client: name}
		if match.IsValid() {
			filter.Client = match.String()
		}
		page, err := f.Reader.Queries(ctx, filter)
		if err == nil && len(page.Queries) > 0 {
			row := page.Queries[0]
			st, _ := pageStatus(row.Status)
			var ede uint16
			if row.EDE != nil {
				ede = uint16(*row.EDE) //nolint:gosec // EDE codes are 16-bit
			}
			r := reasonOf(st, ede, row.EDE != nil, row.List, row.Rule)
			r.Time = row.TS
			return r, true, ""
		}
	}
	if f.Tail != nil {
		// The browser can be faster than the collector goroutine that
		// publishes the query's event: look once more after a moment.
		t := time.NewTimer(tailRetry)
		defer t.Stop()
		select {
		case <-ctx.Done():
		case <-t.C:
			if r, ok := f.scanTail(match, name, domain, since); ok {
				return r, true, ""
			}
		}
	}
	return Reason{}, false, noteNotFound
}

// tailRetry is the pause before the second look at the live tail.
const tailRetry = 100 * time.Millisecond

// scanTail finds the newest page-status event for domain of match (when
// valid) or the client name (when not empty) at or after since.
func (f *QuerylogFinder) scanTail(match netip.Addr, name, domain string, since time.Time) (Reason, bool) {
	evs := f.Tail.Recent(tailScan, 0)
	for i := len(evs) - 1; i >= 0; i-- {
		ev := evs[i]
		if ev.Time.Before(since) {
			break
		}
		if _, ok := pageStatus(ev.Status.String()); !ok || strings.TrimSuffix(ev.Name, ".") != domain {
			continue
		}
		if match.IsValid() && ev.Client.Unmap() != match || name != "" && ev.ClientName != name {
			continue
		}
		r := reasonOf(ev.Status, ev.EDE, ev.HasEDE, ev.List, ev.Rule)
		r.Time = ev.Time
		return r, true
	}
	return Reason{}, false
}
