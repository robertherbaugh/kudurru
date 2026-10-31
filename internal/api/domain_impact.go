package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/blocking"
	"github.com/robertherbaugh/kudurru/internal/blocking/matcher"
	"github.com/robertherbaugh/kudurru/internal/domainlist"
	"github.com/robertherbaugh/kudurru/internal/serving"
)

// DomainRegexImpact describes one concrete name, never an entire regex language
// or the final DNS decision of an unidentified client's selected policy.
type DomainRegexImpact struct {
	Name       string           `json:"name"`
	Action     string           `json:"action" enum:"allow,deny"`
	Expression string           `json:"expression"`
	Matches    bool             `json:"matches" doc:"The selected expression matches this normalized name using the production regex engine."`
	Enabled    bool             `json:"enabled"`
	Everyone   bool             `json:"everyone"`
	Groups     []string         `json:"groups" nullable:"false"`
	Overlaps   []DomainOverride `json:"overlaps" nullable:"false" doc:"Opposing per-list rules for an A/IN question without client identity. This is not a final client policy decision."`
	Incomplete bool             `json:"incomplete" doc:"Some lists or matches exceeded the diagnostic work/output bounds."`
}

type domainImpactInput struct {
	ID   string `path:"id"`
	Name string `query:"name" minLength:"1" maxLength:"255" required:"true"`
}

type domainImpactOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *DomainRegexImpact
}

func (s *Server) registerDomainImpact(action domainlist.Action) {
	operationID := "explainAllowRegex"
	if action == domainlist.Deny {
		operationID = "explainDenyRegex"
	}
	op := operation(http.MethodGet, "/"+string(action)+"list/{id}/impact", operationID, "Test a regex entry against one domain and opposing lists", "domains")
	op.Description = "Bounded name-overlap evidence from the same captured entry and compiled lists. Uses an A/IN question without client identity; selected policies, schedules, modifiers and other rules still determine actual blocking. Does not issue a DNS query or change data."
	register(s, op, perm{resource: "blocklists", verb: "read"}, func(ctx context.Context, in *domainImpactInput) (*domainImpactOutput, error) {
		return s.domainRegexImpact(ctx, action, in)
	})
}

func (s *Server) domainRegexImpact(ctx context.Context, action domainlist.Action, in *domainImpactInput) (*domainImpactOutput, error) {
	if ctx == nil || ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if !boundedSourceString(in.ID, maxObjectReadCursor) {
		return nil, errInvalid("invalid domain entry ID")
	}
	name := dns.CanonicalName(strings.TrimSpace(in.Name))
	if len(in.Name) > 255 || strings.ContainsAny(name, "*\\ \t\r\n") {
		return nil, errInvalid("name must be one concrete DNS domain")
	}
	if _, valid := dns.IsDomainName(name); !valid || name == "." {
		return nil, errInvalid("name must be one concrete DNS domain")
	}
	var entry *DomainEntry
	var lists *blocking.Snapshot
	var original serving.Identity
	if s.capturedConvenienceReads() {
		var err error
		original, err = s.convenienceIdentity(ctx)
		if err != nil {
			return nil, err
		}
		view, err := s.getCapturedDomainEntry(ctx, action, in.ID)
		if err != nil {
			return nil, err
		}
		objects, base, err := s.capturedObjects(ctx)
		if err != nil || !base.Same(original.Base) {
			return nil, objectReadUnavailable()
		}
		entry, lists = view.Body, objects.Lists()
	} else {
		e, set, err := s.findDomainEntry(ctx, action, in.ID)
		if err != nil {
			return nil, err
		}
		lists = s.legacyDomainLists()
		view := s.domainViewWithLists(e, set, lists)
		entry = &view
	}
	if entry == nil || lists == nil {
		return nil, objectReadUnavailable()
	}
	if entry.Type != "regex" || entry.Kind == "exception" {
		return nil, errInvalid("impact requires an allowlist or denylist regex entry")
	}
	if len(entry.Value) > 1024 {
		return nil, errInvalid("expression exceeds the diagnostic bound")
	}
	expression, err := matcher.CompileRegex(entry.Value)
	if err != nil {
		return nil, objectReadUnavailable()
	}
	result := &DomainRegexImpact{Name: name, Action: string(action), Expression: entry.Value, Matches: expression.MatchString(blocking.NormalizeName(name)), Enabled: entry.Enabled, Everyone: entry.Everyone, Groups: entry.Groups, Overlaps: []DomainOverride{}}
	if result.Matches {
		names, incomplete := lists.ListNamesBounded(256)
		result.Incomplete = incomplete
		own := map[string]bool{}
		for _, copy := range entry.Lists {
			own[copy.List] = true
		}
		remainingBytes := 128 << 10
		for _, list := range names {
			if ctx.Err() != nil {
				return nil, objectReadUnavailable()
			}
			if own[list] {
				continue
			}
			var hits map[string]string
			if action == domainlist.Deny {
				hits = lists.ContainsAllow(name, list)
			} else {
				hits = lists.Contains(name, list)
			}
			rule, found := hits[list]
			if !found {
				continue
			}
			if len(result.Overlaps) >= 64 || len(list) > 1024 || len(rule) > 4096 || len(list)+len(rule) > remainingBytes {
				result.Incomplete = true
				continue
			}
			remainingBytes -= len(list) + len(rule)
			result.Overlaps = append(result.Overlaps, DomainOverride{List: list, Rule: rule})
		}
	}
	if ctx.Err() != nil {
		return nil, objectReadUnavailable()
	}
	if s.capturedConvenienceReads() {
		final, err := s.convenienceIdentity(ctx)
		if err != nil || !final.Base.Same(original.Base) || final.Publication != original.Publication || final.Dependencies != original.Dependencies {
			return nil, objectReadUnavailable()
		}
		if _, base, err := s.capturedObjects(ctx); err != nil || !base.Same(original.Base) {
			return nil, objectReadUnavailable()
		}
	}
	return &domainImpactOutput{CacheControl: "no-store", Body: result}, nil
}
