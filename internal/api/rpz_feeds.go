package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/robertherbaugh/kudurru/internal/rpz"
)

// RPZFeedSource reports the RPZ feeds of the responding node (rpz.Manager).
// It is node-local acquisition state, read at the time of the request: it is
// not the serving cut a DNS answer or a complete policy read was bound to.
type RPZFeedSource interface {
	Statuses() []rpz.Status
	ListStatuses(namespace, name string) ([]rpz.Status, error)
}

// RPZFeed is one enabled RPZ source of a BlockList on the responding node
// (D391). Every node transfers its own copy, so two nodes can disagree; shared
// BlockList status never describes feeds.
type RPZFeed struct {
	Node      string `json:"node" doc:"The node that answered; feed state is per node."`
	Namespace string `json:"namespace"`
	List      string `json:"list" doc:"BlockList name."`
	Source    string `json:"source" doc:"Source name, or rpz-<index> when the source has none."`
	Origin    string `json:"origin" doc:"Policy-zone origin as configured."`
	// State: pending (no copy yet, no failure), serving (an unexpired copy is
	// available), expired (the last copy reached its SOA expire), unavailable
	// (no usable copy: configuration, credential or transfer failure).
	// Queries whose policy selects a feed that is not serving answer SERVFAIL.
	State     string `json:"state" enum:"pending,serving,expired,unavailable"`
	Available bool   `json:"available" doc:"Whether DNS on this node can apply the feed now."`
	Reason    string `json:"reason,omitempty"`
	Message   string `json:"message,omitempty"`
	// Serial and the counts are present once the node holds a copy, also
	// after that copy expired.
	Serial          *uint32    `json:"serial,omitempty" format:"int64" minimum:"0" maximum:"4294967295"`
	Primary         string     `json:"primary,omitempty" doc:"Primary that served the copy."`
	LastConfirmedAt *time.Time `json:"lastConfirmedAt,omitempty" doc:"Last transfer or SOA check that confirmed the copy."`
	ExpiresAt       *time.Time `json:"expiresAt,omitempty" doc:"Absolute expiry of the copy (confirmation plus SOA expire)."`
	NextAttemptAt   *time.Time `json:"nextAttemptAt,omitempty"`
	// LastError is the last failed refresh; a serving copy can carry one
	// while it stays within its expiry.
	LastError          string             `json:"lastError,omitempty"`
	ConfigurationError string             `json:"configurationError,omitempty"`
	Counts             *RPZFeedCounts     `json:"counts,omitempty"`
	Rules              *RPZRuleCounts     `json:"rules,omitempty"`
	Ignored            []RPZIgnoredReason `json:"ignored" nullable:"false" doc:"Ignored policy owners per reason; complete counts, bounded examples."`
}

// RPZFeedCounts are the compiler totals of the copy. Rules are compiled
// policy owners; Invalid and Unsupported owners are ignored (see Ignored);
// Excluded are switched off by spec.sources[].exclude; StaleExcludes are
// exclusions that match no owner; Ignored records are DNSSEC, apex or
// duplicate records that carry no policy.
type RPZFeedCounts struct {
	Records        int `json:"records"`
	Rules          int `json:"rules"`
	Invalid        int `json:"invalid"`
	Unsupported    int `json:"unsupported"`
	Excluded       int `json:"excluded"`
	IgnoredRecords int `json:"ignoredRecords"`
	StaleExcludes  int `json:"staleExcludes"`
}

// RPZRuleCounts splits the compiled rules by trigger and by action.
type RPZRuleCounts struct {
	QName      int `json:"qname"`
	ClientIP   int `json:"clientIP"`
	ResponseIP int `json:"responseIP"`
	NXDOMAIN   int `json:"nxdomain"`
	NODATA     int `json:"nodata"`
	Passthru   int `json:"passthru"`
	Drop       int `json:"drop"`
	TCPOnly    int `json:"tcpOnly"`
	LocalData  int `json:"localData"`
}

// RPZIgnoredReason accounts for the owners ignored for one reason.
type RPZIgnoredReason struct {
	Reason      string   `json:"reason" doc:"apex-policy, resolver-ns-provenance, invalid-address-trigger, unknown-reserved-trigger, nonterminal-wildcard, dname-action, invalid-cname-action, unknown-reserved-action or nonterminal-cname-wildcard."`
	Unsupported bool     `json:"unsupported" doc:"True for well-formed policy kudurru does not implement, false for a malformed owner."`
	Count       int      `json:"count"`
	Examples    []string `json:"examples" nullable:"false" doc:"Up to five owner names."`
}

// maxRPZFeedText bounds the free-text fields copied from transfer errors.
const maxRPZFeedText = 1024

func boundRPZText(s string) string {
	if len(s) <= maxRPZFeedText {
		return s
	}
	s = s[:maxRPZFeedText]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s + "…"
}

func rpzFeed(node string, st rpz.Status) RPZFeed {
	namespace, name, _ := strings.Cut(st.List, "/")
	out := RPZFeed{Node: node, Namespace: namespace, List: name, Source: st.Source, Origin: st.Origin, Available: st.Available,
		Primary: st.Primary, LastError: boundRPZText(st.Error), ConfigurationError: boundRPZText(st.ConfigurationError), Ignored: []RPZIgnoredReason{}}
	if !st.NextRefresh.IsZero() {
		next := st.NextRefresh
		out.NextAttemptAt = &next
	}
	hasCopy := !st.ConfirmedAt.IsZero()
	if hasCopy {
		serial, confirmed, expires := st.Serial, st.ConfirmedAt, st.ExpiresAt
		out.Serial, out.LastConfirmedAt, out.ExpiresAt = &serial, &confirmed, &expires
		s := st.Stats
		out.Counts = &RPZFeedCounts{Records: s.Records, Rules: s.Rules, Invalid: s.Invalid, Unsupported: s.Unsupported,
			Excluded: s.Excluded, IgnoredRecords: s.Ignored, StaleExcludes: s.StaleExcludes}
		r := st.Rules
		out.Rules = &RPZRuleCounts{QName: r.QName, ClientIP: r.ClientIP, ResponseIP: r.ResponseIP, NXDOMAIN: r.NXDOMAIN, NODATA: r.NODATA,
			Passthru: r.Passthru, Drop: r.Drop, TCPOnly: r.TCPOnly, LocalData: r.LocalData}
		for _, ig := range st.IgnoredReasons() {
			examples := ig.Examples
			if examples == nil {
				examples = []string{}
			}
			out.Ignored = append(out.Ignored, RPZIgnoredReason{Reason: ig.Reason, Unsupported: ig.Unsupported, Count: ig.Count, Examples: examples})
		}
	}
	switch {
	case st.ConfigurationError != "":
		out.State, out.Reason, out.Message = "unavailable", "ConfigurationUnavailable", out.ConfigurationError
	case st.Available:
		out.State = "serving"
		if st.Error != "" {
			out.Reason, out.Message = "RPZTransfer", "The last refresh failed; this node keeps applying the copy until it expires: "+out.LastError
		}
	case hasCopy:
		out.State, out.Reason, out.Message = "expired", "RPZExpired", "The last copy has expired; queries whose policy selects this list answer SERVFAIL on this node."
		if st.Error != "" {
			out.Message += " Last refresh error: " + out.LastError
		}
	case st.Error != "":
		out.State, out.Reason, out.Message = "unavailable", "RPZTransfer", "No copy has been transferred; queries whose policy selects this list answer SERVFAIL on this node: "+out.LastError
	default:
		out.State, out.Reason, out.Message = "pending", "TransferPending", "Waiting for this node's first complete transfer."
	}
	return out
}

type rpzFeedListInput struct {
	Namespace string `query:"namespace" doc:"Only feeds of lists in this namespace; empty means every namespace."`
}

type rpzFeedsOutput struct {
	Body *List[RPZFeed]
}

func (s *Server) registerRPZFeeds() {
	register(s, operation(http.MethodGet, "/blocklists/_rpz", "listRPZFeeds", "RPZ feed state on this node", "blocklists"),
		perm{resource: "blocklists", verb: "read"}, s.listRPZFeeds)
	register(s, operation(http.MethodGet, "/blocklists/{namespace}/{name}/rpz", "getBlockListRPZFeeds", "RPZ feed state of one BlockList on this node", "blocklists"),
		perm{resource: "blocklists", verb: "read"}, s.getBlockListRPZFeeds)
}

func (s *Server) listRPZFeeds(_ context.Context, in *rpzFeedListInput) (*rpzFeedsOutput, error) {
	if s.opts.RPZ == nil {
		return nil, errNotImplemented("RPZ feed state")
	}
	items := []RPZFeed{}
	for _, st := range s.opts.RPZ.Statuses() {
		feed := rpzFeed(s.opts.NodeName, st)
		if in.Namespace == "" || feed.Namespace == in.Namespace {
			items = append(items, feed)
		}
	}
	return &rpzFeedsOutput{Body: newList(items, "")}, nil
}

func (s *Server) getBlockListRPZFeeds(_ context.Context, in *nsObjectInput) (*rpzFeedsOutput, error) {
	if s.opts.RPZ == nil {
		return nil, errNotImplemented("RPZ feed state")
	}
	statuses, err := s.opts.RPZ.ListStatuses(in.Namespace, in.Name)
	switch {
	case errors.Is(err, rpz.ErrListNotConfigured):
		return nil, errNotFound("blocklist")
	case err != nil:
		return nil, errUnavailable("this node has no configuration snapshot yet")
	}
	items := make([]RPZFeed, 0, len(statuses))
	for _, st := range statuses {
		items = append(items, rpzFeed(s.opts.NodeName, st))
	}
	return &rpzFeedsOutput{Body: newList(items, "")}, nil
}

// rpzFeedMessages adds one diagnosis per feed that is not serving cleanly and
// one per feed with ignored owners. It reads the node's feed manager now, like
// the other operational samples.
func (s *Server) rpzFeedMessages(add func(DiagnosisMessage) error) error {
	if s.opts.RPZ == nil {
		return nil
	}
	for _, st := range s.opts.RPZ.Statuses() {
		feed := rpzFeed(s.opts.NodeName, st)
		object := "BlockList/" + st.List
		if feed.Message != "" {
			severity := "error"
			switch {
			case feed.State == "pending":
				severity = "info"
			case feed.Available:
				severity = "warning"
			}
			if err := add(DiagnosisMessage{Severity: severity, Source: "rpz", Reason: feed.Reason, Object: object,
				Message: "RPZ feed " + feed.Origin + " (source " + feed.Source + "): " + feed.Message,
				Hint:    "This node's feed state is in GET /blocklists/{namespace}/{name}/rpz and kudurru lists rpz."}); err != nil {
				return err
			}
		}
		if feed.Counts != nil && feed.Counts.Invalid+feed.Counts.Unsupported > 0 {
			reasons := make([]string, 0, len(feed.Ignored))
			for _, ig := range feed.Ignored {
				reasons = append(reasons, ig.Reason)
			}
			if err := add(DiagnosisMessage{Severity: "info", Source: "rpz", Reason: "RPZRulesIgnored", Object: object,
				Message: "RPZ feed " + feed.Origin + " (source " + feed.Source + "): " + strconv.Itoa(feed.Counts.Unsupported) + " unsupported and " + strconv.Itoa(feed.Counts.Invalid) + " invalid policy owners are ignored (" + strings.Join(reasons, ", ") + ").",
				Hint:    "Other rules of the feed still apply. See docs/deploy/rpz.md for the supported triggers and actions."}); err != nil {
				return err
			}
		}
	}
	return nil
}
