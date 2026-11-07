package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/robertherbaugh/kudurru/internal/auth"
)

// VIPStreamEvent is one local notification, never physical ownership or write
// admission. A source status has Reason and no fabricated State/Previous.
type VIPStreamEvent struct {
	Node      string    `json:"node"`
	Session   string    `json:"session,omitempty"`
	Sequence  uint64    `json:"sequence"`
	VIP       string    `json:"vip,omitempty"`
	VRID      int       `json:"vrid,omitempty"`
	Previous  string    `json:"previous,omitempty"`
	State     string    `json:"state,omitempty"`
	Priority  int       `json:"priority,omitempty"`
	At        time.Time `json:"at"`
	ExpiresAt time.Time `json:"expiresAt"`
	Reason    string    `json:"reason,omitempty"`
}

// VIPEventHub bounds local fanout to 16 streams, 32 events each. It retains no
// replay history. Overflow closes the affected stream, never blocks the source.
type VIPEventHub struct {
	mu          sync.Mutex
	next        uint64
	subs        map[uint64]chan VIPStreamEvent
	unavailable bool
}

// Subscribe acquires a bounded local stream and an idempotent release function.
func (h *VIPEventHub) Subscribe(ctx context.Context) (<-chan VIPStreamEvent, func(), error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ctx.Err() != nil || h.unavailable || len(h.subs) >= 16 {
		return nil, nil, errors.New("VIP stream unavailable")
	}
	if h.subs == nil {
		h.subs = map[uint64]chan VIPStreamEvent{}
	}
	h.next++
	id := h.next
	ch := make(chan VIPStreamEvent, 32)
	h.subs[id] = ch
	release := func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if current, ok := h.subs[id]; ok {
			delete(h.subs, id)
			close(current)
		}
	}
	stop := context.AfterFunc(ctx, release)
	return ch, func() { stop(); release() }, nil
}

// Publish returns the number of slow subscribers explicitly disconnected.
func (h *VIPEventHub) Publish(event VIPStreamEvent) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	dropped := 0
	for id, ch := range h.subs {
		select {
		case ch <- event:
		default:
			delete(h.subs, id)
			close(ch)
			dropped++
		}
	}
	return dropped
}

// Interrupt closes subscriptions after a source discontinuity or shutdown.
// A new request may subscribe, but old notification history is never replayed.
func (h *VIPEventHub) Interrupt() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}

// SetAvailable controls source acquisition availability, never supervisor or
// DNS readiness. A failed source closes current streams and refuses new ones.
func (h *VIPEventHub) SetAvailable(available bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.unavailable = !available
	if available {
		return
	}
	for id, ch := range h.subs {
		delete(h.subs, id)
		close(ch)
	}
}

type vipStreamInput struct {
	LastEventID string `header:"Last-Event-ID"`
	Cursor      string `query:"cursor"`
}

func (s *Server) registerVIPEvents() {
	op := operation(http.MethodGet, "/edge/vip/stream", "vipEventStream", "Local observed VIP transitions (Server-Sent Events)", "cluster")
	op.Description = "Bounded local notifications: event vip.master_changed contains VIP, node, VRID, previous/state, original transition time and source session/sequence. Source diagnostics use vip.observation_status without a fabricated transition. No historical replay; cursors are refused. Observer startup baselines existing history; source restart/gaps close streams. Each emission rechecks original request credentials and edge.read permission; no writer authority is granted."
	op.Responses = map[string]*huma.Response{"200": {Description: "Live local notification stream", Content: map[string]*huma.MediaType{"text/event-stream": {Schema: &huma.Schema{Type: "string"}}}}}
	register(s, op, perm{resource: "edge", verb: "read"}, s.vipEventStream)
}

func (s *Server) vipEventStream(ctx context.Context, in *vipStreamInput) (*huma.StreamResponse, error) {
	if in.LastEventID != "" || in.Cursor != "" {
		return nil, errInvalid("VIP notification replay is not supported")
	}
	if s.opts.VIPEvents == nil {
		return nil, errUnavailable("local VIP observation is not enabled")
	}
	events, release, err := s.opts.VIPEvents.Subscribe(ctx)
	if err != nil {
		return nil, errUnavailable("VIP notification source or stream capacity unavailable")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer release()
		_, w := humago.Unwrap(hctx)
		rc := http.NewResponseController(w)
		stopWrite := context.AfterFunc(ctx, func() { _ = rc.SetWriteDeadline(time.Now()) })
		defer stopWrite()
		// Do not use require's cached clusterAllowed fast path. In complete
		// serving, the original authorizer rechecks the same retained raw proof
		// and auth view at current time; it cannot recapture a replacement.
		allowed := func() bool {
			if ctx.Err() != nil {
				return false
			}
			p, ok := auth.PrincipalFrom(ctx)
			if !ok || p.MustChangePassword {
				return false
			}
			checkCtx, stop := context.WithTimeout(ctx, 2*time.Second)
			defer stop()
			decision, err := s.authorize(checkCtx, p, "edge", "read", clusterScope, "")
			_, current := auth.PrincipalFrom(ctx)
			return checkCtx.Err() == nil && current && err == nil && decision.Allowed
		}
		write := func(frame string, event *VIPStreamEvent) bool {
			if !allowed() {
				return false
			}
			now := time.Now()
			until := now.Add(sseWriteTimeout)
			if deadline, ok := ctx.Deadline(); ok && deadline.Before(until) {
				until = deadline
			}
			if event != nil {
				if event.At.IsZero() || now.Before(event.At) || !now.Before(event.ExpiresAt) {
					return false
				}
				if event.ExpiresAt.Before(until) {
					until = event.ExpiresAt
				}
			}
			if ctx.Err() != nil || rc.SetWriteDeadline(until) != nil {
				return false
			}
			_, err := fmt.Fprint(w, frame)
			return err == nil && rc.Flush() == nil
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("X-Accel-Buffering", "no")
		if !write(": connected\n\n", nil) {
			return
		}
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		beats := 0
		for {
			select {
			case <-ctx.Done():
				return
			case event, ok := <-events:
				if !ok {
					return
				}
				data, err := json.Marshal(event)
				if err != nil || len(data) > 4096 {
					return
				}
				name := "vip.master_changed"
				if event.Reason != "" {
					name = "vip.observation_status"
				}
				if !write("event: "+name+"\ndata: "+string(data)+"\n\n", &event) {
					return
				}
			case <-ticker.C:
				if !allowed() {
					return
				}
				beats++
				if beats%15 == 0 && !write(": heartbeat\n\n", nil) {
					return
				}
			}
		}
	}}, nil
}
