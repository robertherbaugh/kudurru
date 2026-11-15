package internalapi

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/robertherbaugh/kudurru/internal/cluster"
)

// EventFanout delivers best-effort hints to every peer using four bounded
// workers. Enqueue is nonblocking. Failed or dropped hints are observable;
// receivers' independent refresh timers recover without replay or loops.
type EventFanout struct {
	membership cluster.Membership
	send       func(context.Context, string, Event) error
	log        *slog.Logger
	queue      chan Event
	results    *prometheus.CounterVec
	mu         sync.Mutex
	pending    map[Event]bool
}

// NewEventFanout creates the peer queue. A nil client makes fan-out a no-op.
func NewEventFanout(m cluster.Membership, c *Client, reg prometheus.Registerer, logger *slog.Logger) *EventFanout {
	if logger == nil {
		logger = slog.Default()
	}
	f := &EventFanout{membership: m, log: logger, queue: make(chan Event, 128), pending: make(map[Event]bool), results: prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kudurru_peer_events_total", Help: "Best-effort peer hint delivery by outcome.",
	}, []string{"result"})}
	if c != nil {
		f.send = c.SendEvent
	}
	if reg != nil {
		reg.MustRegister(f.results)
	}
	return f
}

// Enqueue coalesces identical hints already waiting for delivery.
func (f *EventFanout) Enqueue(e Event) {
	if f.send == nil || !e.valid() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pending[e] {
		return
	}
	select {
	case f.queue <- e:
		f.pending[e] = true
	default:
		f.results.WithLabelValues("dropped").Inc()
	}
}

// Run joins all workers before returning so no delivery outlives shutdown.
func (f *EventFanout) Run(ctx context.Context) error {
	var workers sync.WaitGroup
	for range 4 {
		workers.Go(func() {
			for {
				select {
				case <-ctx.Done():
					return
				case e := <-f.queue:
					f.deliver(ctx, e)
					f.mu.Lock()
					delete(f.pending, e)
					f.mu.Unlock()
				}
			}
		})
	}
	workers.Wait()
	return nil
}

func (f *EventFanout) deliver(ctx context.Context, e Event) {
	if f.membership == nil || f.send == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	peers, err := f.membership.Peers(ctx)
	if err != nil {
		f.results.WithLabelValues("membership_error").Inc()
		f.log.Warn("peer event membership unavailable", "error", err)
		return
	}
	seen := make(map[string]bool)
	for _, peer := range peers {
		if peer.Self || peer.Address == "" || seen[peer.Address] {
			continue
		}
		if ctx.Err() != nil {
			f.results.WithLabelValues("timeout").Inc()
			return
		}
		seen[peer.Address] = true
		if err := f.send(ctx, peer.Address, e); err != nil {
			f.results.WithLabelValues("error").Inc()
			f.log.Debug("peer event delivery failed", "peer", peer.Name, "error", err)
		} else {
			f.results.WithLabelValues("sent").Inc()
		}
	}
}
