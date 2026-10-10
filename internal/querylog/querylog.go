// Package querylog defines the query event, the non-blocking collector the
// log observer writes to, and the Sink contract every external sink
// implements (docs/architecture.md §11.2).
package querylog

import (
	"context"
	"encoding/json"
	"net/netip"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/robertherbaugh/kudurru/internal/pipeline"
)

// Event is one answered (or dropped) query. Privacy transforms are applied
// once, by the collector, before any sink sees the event.
type Event struct {
	Time     time.Time       `json:"ts"`
	Latency  time.Duration   `json:"latency"`
	Node     string          `json:"node"`
	Proto    pipeline.Proto  `json:"proto"`
	Client   netip.Addr      `json:"client"`
	Name     string          `json:"name"`
	QType    uint16          `json:"qtype"`
	QClass   uint16          `json:"qclass"`
	Status   pipeline.Status `json:"status"`
	Rcode    int             `json:"rcode"`
	Upstream string          `json:"upstream,omitempty"`
	List     string          `json:"list,omitempty"`
	Rule     string          `json:"rule,omitempty"`
	Answers  int             `json:"answers"`
	Flags    Flags           `json:"flags"`
}

// Flags are the boolean facets stored in one column.
type Flags struct {
	RD, DO, TC, AD, CD bool
	Cached, Stale      bool
}

// Collector accepts events without blocking the query path.
type Collector interface {
	// Enqueue returns false when the event was dropped (queue full).
	Enqueue(ev *Event) bool
}

// Sink is any destination for batches of events. The host owns queue
// sizing, drop counters, retry and privacy; sinks only write.
type Sink interface {
	Name() string
	Open(ctx context.Context, cfg json.RawMessage, reg prometheus.Registerer) error
	// Write is called from a dedicated goroutine, never from the query path.
	Write(batch []*Event) error
	Flush(ctx context.Context) error
	Close() error
}
