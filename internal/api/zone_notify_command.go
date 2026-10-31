package api

import (
	"context"
	"net/http"

	"github.com/robertherbaugh/kudurru/internal/audit"
)

// ZoneNotifyCommand admits durable manual NOTIFY intent in one current command.
// Success means accepted/coalesced, not sent, published or acknowledged. Targets
// are bounded configured literal addresses; dynamic NS recipients are resolved
// only by the background sender.
type ZoneNotifyCommand func(context.Context, string) (*NotifyResult, error)

func (s *Server) notifyZoneCommand(ctx context.Context, in *zonePathInput) (*notifyOutput, error) {
	if s.opts.ZoneNotifyCommand == nil || ctx == nil || ctx.Err() != nil || in == nil {
		return nil, errUnavailable("current NOTIFY command unavailable")
	}
	result, err := s.opts.ZoneNotifyCommand(ctx, in.Zone)
	s.recordAction(ctx, "zones.transfer", audit.Resource{Kind: "DNSZone", Name: in.Zone}, err, nil, nil)
	if err != nil {
		return nil, err
	}
	if result == nil || result.Zone != in.Zone || len(result.Targets) > 64 {
		return nil, errUnavailable("current NOTIFY command result unavailable")
	}
	out := *result
	out.Targets = make([]string, len(result.Targets))
	for i, target := range result.Targets {
		if len(target) > 512 {
			return nil, errUnavailable("current NOTIFY target exceeds bounds")
		}
		out.Targets[i] = target
	}
	return &notifyOutput{Status: http.StatusAccepted, Body: &out}, nil
}
