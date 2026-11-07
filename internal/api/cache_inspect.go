package api

import (
	"context"
	"net/netip"
	"strings"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/cache"
)

// CacheInspectRequest selects one raw default-route response. Client only
// derives the configured ECS partition; it does not identify a logged query.
type CacheInspectRequest struct {
	Name   string
	Type   uint16
	DO, CD bool
	Client netip.Addr
}

// CacheInspectRead observes the original request's cache/resolver owners.
// Implementations must never perform DNS I/O or expose arbitrary stored keys.
type CacheInspectRead func(context.Context, CacheInspectRequest) (cache.Lookup, bool, bool, error)

type cacheInspectInput struct {
	Name   string `query:"name" required:"true" minLength:"1" maxLength:"255" doc:"Exact DNS name; no wildcard or cache enumeration."`
	Type   string `query:"type" default:"A" maxLength:"16" doc:"DNS type mnemonic."`
	DO     bool   `query:"do" doc:"DNSSEC OK bit of the raw cache key."`
	CD     bool   `query:"cd" doc:"Checking Disabled bit of the raw cache key."`
	Client string `query:"client" maxLength:"45" doc:"Address used only to derive ECS; default 127.0.0.1."`
}

// CacheInspection is metadata for one raw response, not a policy evaluation.
// It excludes answer contents, stored client subnets, and upstream endpoints.
type CacheInspection struct {
	Name      string              `json:"name"`
	Type      string              `json:"type"`
	Node      string              `json:"node"`
	Cacheable bool                `json:"cacheable"`
	Found     bool                `json:"found"`
	Entry     *CacheEntryMetadata `json:"entry,omitempty"`
	Took
}

// CacheEntryMetadata contains bounded, detached scalar cache evidence.
type CacheEntryMetadata struct {
	Kind             string  `json:"kind" enum:"positive,negative"`
	State            string  `json:"state" enum:"fresh,stale"`
	AgeSeconds       float64 `json:"ageSeconds"`
	TTLSeconds       float64 `json:"ttlSeconds" doc:"Original stored lifetime."`
	RemainingSeconds float64 `json:"remainingSeconds" doc:"Zero for stale entries."`
	WireBytes        int     `json:"wireBytes"`
	Validation       string  `json:"validation"`
}

type cacheInspectOutput struct {
	CacheControl string `header:"Cache-Control"`
	Body         *CacheInspection
}

func (s *Server) inspectCache(ctx context.Context, in *cacheInspectInput) (*cacheInspectOutput, error) {
	name := dns.CanonicalName(strings.TrimSpace(in.Name))
	if len(in.Name) > 255 || strings.TrimSpace(in.Name) == "" {
		return nil, errInvalid("name: invalid domain name")
	}
	if _, ok := dns.IsDomainName(name); !ok {
		return nil, errInvalid("name: invalid domain name")
	}
	typ := strings.ToUpper(in.Type)
	qtype, ok := dns.StringToType[typ]
	if !ok {
		return nil, errInvalid("type: unknown query type")
	}
	client := netip.MustParseAddr("127.0.0.1")
	if in.Client != "" {
		var err error
		client, err = netip.ParseAddr(in.Client)
		if err != nil || client.Zone() != "" {
			return nil, errInvalid("client: invalid address")
		}
		client = client.Unmap()
	}
	if s.opts.CacheInspect == nil {
		return nil, errUnavailable("captured cache inspection unavailable")
	}
	value, found, eligible, err := s.opts.CacheInspect(ctx, CacheInspectRequest{Name: name, Type: qtype, DO: in.DO, CD: in.CD, Client: client})
	if err != nil || ctx.Err() != nil || found && !eligible {
		return nil, errUnavailable("captured cache inspection unavailable")
	}
	out := &CacheInspection{Name: name, Type: typ, Node: s.opts.NodeName, Cacheable: eligible, Found: found}
	if found {
		state := "fresh"
		if value.State == cache.Stale {
			state = "stale"
		}
		out.Entry = &CacheEntryMetadata{Kind: value.Kind.String(), State: state, AgeSeconds: value.Age.Seconds(), TTLSeconds: value.TTL.Seconds(), RemainingSeconds: max((value.TTL - value.Age).Seconds(), 0), WireBytes: len(value.Wire), Validation: value.Metadata.Security.State.String()}
	}
	return &cacheInspectOutput{CacheControl: "no-store", Body: out}, nil
}
