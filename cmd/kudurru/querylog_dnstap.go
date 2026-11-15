package main

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/robertherbaugh/kudurru/internal/audit"
	"github.com/robertherbaugh/kudurru/internal/querylog/sinks/dnstap"
)

// AuditQuerylogRawExport is the audit action a node records before a raw
// dnstap sink sends its first message (D372).
const AuditQuerylogRawExport = "querylog.raw.export"

// errRawAuditUnavailable keeps a raw sink closed until the node's audit
// trail is mounted.
var errRawAuditUnavailable = errors.New("the audit trail of this node is not ready")

// rawExportAudit records the start of every raw dnstap sink on this node.
// The query-log runtime is built before the audit recorders, so they are
// bound later (bind); a raw sink that opens before then stays closed and
// the sink host tries again with backoff. Nothing raw is sent without an
// audit event: the sink does not open when Record fails.
type rawExportAudit struct {
	node     string
	recorder atomic.Pointer[audit.Recorder]
	now      func() time.Time
}

func (a *rawExportAudit) bind(r audit.Recorder) {
	if a != nil && r != nil {
		a.recorder.Store(&r)
	}
}

// record is the dnstap Options.Audit of this node.
func (a *rawExportAudit) record(ctx context.Context, start dnstap.RawStart) error {
	r := a.recorder.Load()
	if r == nil {
		return errRawAuditUnavailable
	}
	now := time.Now
	if a.now != nil {
		now = a.now
	}
	resource := audit.Resource{Kind: "querylog-sink", Name: start.Sink}
	// The actor is the node: the administrator who saved the setting is
	// on the settings change (querylog.raw); this event says where and
	// when raw messages began to flow.
	return (*r).Record(ctx, audit.Sanitize(audit.Event{
		Time: now(), Node: a.node, Actor: audit.Actor{Kind: "System", Name: "query-log", Auth: "host"},
		Action: AuditQuerylogRawExport, Resource: resource, Result: audit.ResultSuccess, Authz: "settings",
		Reason: "querylog.sinks entry " + start.Sink + " sends raw dnstap to " + start.Target,
		After:  audit.Document(resource.Kind, start),
	}))
}
