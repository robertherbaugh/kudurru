package main

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/robertherbaugh/kudurru/ee/edge/vip"
	"github.com/robertherbaugh/kudurru/internal/api"
	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/config"
)

// keepalivedVIPObservation mounts only local notification reads and audit/SSE.
// No Provider or SetWeight capability crosses the API boundary.
func keepalivedVIPObservation(cfg *config.Config, recorder audit.Recorder, logger *slog.Logger) (*api.VIPEventHub, func(context.Context) error, error) {
	settings := cfg.VIP.Observation
	if settings.Provider != "keepalived" {
		return nil, nil, nil
	}
	if cfg.Node.Name == "" || len(cfg.VIP.VIPs) == 0 || len(cfg.VIP.VIPs) > 32 || recorder == nil {
		return nil, nil, errors.New("VIP notifications require host node, VIP mappings and audit recorder")
	}
	if logger == nil {
		logger = slog.Default()
	}
	bindings := make([]vip.VIP, 0, len(cfg.VIP.VIPs))
	for _, binding := range cfg.VIP.VIPs {
		bindings = append(bindings, vip.VIP{Name: binding.Name, Address: binding.Address, VRID: binding.VRID})
	}
	provider := vip.NewKeepalived(settings.RunDir, cfg.Node.Name, bindings)
	provider.Poll = settings.Interval.Std()
	hub := &api.VIPEventHub{}
	hub.SetAvailable(false)
	node := cfg.Node.Name
	auditTimeout := min(settings.Timeout.Std(), 2*time.Second)
	run := func(ctx context.Context) error {
		defer hub.SetAvailable(false)
		return provider.Observe(ctx, settings.MaxAge.Std(), func(ctx context.Context, notice vip.KeepalivedNotice) {
			if ctx.Err() != nil {
				return
			}
			value := api.VIPStreamEvent{Node: node, At: time.Now().UTC(), Session: notice.Session, Sequence: notice.Sequence, Reason: notice.Issue}
			result := audit.ResultSuccess
			resource := audit.Resource{Kind: "vip-observation", Name: node}
			if event := notice.Event; event != nil {
				value.VIP, value.VRID = event.VIP, event.VRID
				value.Previous, value.State = string(event.Previous), string(event.State)
				value.Priority, value.At = event.Priority, event.At
				resource = audit.Resource{Kind: "vip", Name: event.VIP}
			} else if notice.Issue != "source_ready" {
				result = audit.ResultError
			}
			// Preserve the ORIGINAL transition expiry through audit and queue
			// delays. Attach local monotonic time without extending source age.
			now := time.Now()
			value.ExpiresAt = now.Add(value.At.Add(settings.MaxAge.Std()).Sub(now))
			// System observation is authorized only by original host config.
			// Recording does not borrow a user's proof or acquire a writer.
			auditCtx, cancel := context.WithTimeout(ctx, auditTimeout)
			err := recorder.Record(auditCtx, audit.Sanitize(audit.Event{Time: value.At, Node: node, Actor: audit.Actor{Kind: "System", Name: "keepalived-observer", Auth: "host"}, Action: vip.AuditVIPChanged, Resource: resource, Result: result, Authz: "host-config", Reason: notice.Issue, After: audit.Document(resource.Kind, value)}))
			cancel()
			if err != nil {
				logger.Warn("VIP observation audit unavailable")
				hub.SetAvailable(false)
				return
			}
			if notice.Issue != "" && notice.Issue != "source_ready" {
				logger.Warn("VIP notification source changed", "reason", notice.Issue)
				hub.SetAvailable(false)
				return
			}
			if ctx.Err() != nil || !time.Now().Before(value.ExpiresAt) {
				logger.Warn("VIP notification expired before publication")
				hub.SetAvailable(false)
				return
			}
			hub.SetAvailable(true)
			if dropped := hub.Publish(value); dropped != 0 {
				logger.Warn("VIP notification subscribers disconnected", "count", dropped)
			}
		})
	}
	return hub, run, nil
}
