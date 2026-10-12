package internalapi

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
	"github.com/robertherbaugh/kudurru/internal/upstream"
)

// Query-log routes use the same TLS, token and rate limits as peer status.
const (
	PathQuerylog       = "/internal/querylog"
	PathQuerylogStream = "/internal/querylog/stream"
	HeaderQuerylogNode = "X-Kudurru-Node"
)

// QuerylogRequest describes a bounded, local-only read. Scope is deliberately
// absent: peers must never recursively fan out a coordinator's request.
type QuerylogRequest struct {
	Kind    string                `json:"kind"`
	Window  sqlitelog.Window      `json:"window"`
	Step    time.Duration         `json:"step,omitempty"`
	N       int                   `json:"n,omitempty"`
	Blocked bool                  `json:"blocked,omitempty"`
	Filter  sqlitelog.QueryFilter `json:"filter"`
}

// QuerylogResponse is the internal read model, including merge metadata
// that is never exposed by the public API.
type QuerylogResponse struct {
	Summary    *sqlitelog.SummaryShard     `json:"summary,omitempty"`
	Timeseries *sqlitelog.Timeseries       `json:"timeseries,omitempty"`
	Top        *sqlitelog.TopList          `json:"top,omitempty"`
	Upstreams  *sqlitelog.UpstreamShard    `json:"upstreams,omitempty"`
	Clients    *sqlitelog.ClientTimeseries `json:"clients,omitempty"`
	Blocked    *sqlitelog.RecentBlocked    `json:"blocked,omitempty"`
	Queries    *sqlitelog.QueryPage        `json:"queries,omitempty"`
	Servers    []upstream.ServerStatus     `json:"servers,omitempty"`
}

func (s *Server) handleQuerylog(w http.ResponseWriter, r *http.Request) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(statusWriteTimeout))
	var q QuerylogRequest
	decoder := json.NewDecoder(io.LimitReader(strings.NewReader(r.URL.Query().Get("q")), maxHeaderBytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&q); err != nil || q.N < 0 || q.N > sqlitelog.MaxTopN*4 || q.Filter.Limit < 0 || q.Filter.Limit > sqlitelog.MaxLimit || q.Step < 0 {
		http.Error(w, "invalid query-log request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	out, err := s.opts.Querylog(ctx, q)
	if err != nil {
		s.log.Debug("internal query-log read failed", "kind", q.Kind, "err", err)
		http.Error(w, "query log unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(out)
}

// ReadQuerylog reads one peer's local log. The coordinator bounds the whole
// read, including the body, using its context.
func (c *Client) ReadQuerylog(ctx context.Context, addr string, q QuerylogRequest) (*QuerylogResponse, error) {
	b, err := json.Marshal(q)
	if err != nil {
		return nil, err
	}
	resp, err := c.get(ctx, addr, PathQuerylog+"?"+url.Values{"q": {string(b)}}.Encode())
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out QuerylogResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return nil, fmt.Errorf("internalapi: decode query log: %w", err)
	}
	return &out, nil
}

func (s *Server) handleQuerylogStream(w http.ResponseWriter, r *http.Request) {
	after, err := strconv.ParseUint(r.URL.Query().Get("after"), 10, 64)
	if err != nil {
		http.Error(w, "invalid stream cursor", http.StatusBadRequest)
		return
	}
	tail := s.opts.QueryTail()
	if tail == nil {
		http.Error(w, "live tail unavailable", http.StatusServiceUnavailable)
		return
	}
	select {
	case s.streams <- struct{}{}:
		defer func() { <-s.streams }()
	default:
		http.Error(w, "too many streams", http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	events := tail.Subscribe(ctx)
	w.Header().Set(HeaderQuerylogNode, s.opts.Credentials.nodeName)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(ev *querylog.Event) error {
		if ev.ID != 0 && ev.ID <= after {
			return nil
		}
		b, err := json.Marshal(ev)
		if err != nil {
			return err
		}
		_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		after = max(after, ev.ID)
		return rc.Flush()
	}
	_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := fmt.Fprint(w, ": ready\n\n"); err != nil || rc.Flush() != nil {
		return
	}
	if after != 0 {
		for _, ev := range tail.Recent(0, after) {
			if send(ev) != nil {
				return
			}
		}
	}
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok || send(ev) != nil {
				return
			}
		case <-tick.C:
			_ = rc.SetWriteDeadline(time.Now().Add(30 * time.Second))
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}

// QuerylogStream owns a peer's stream. Next skips heartbeats and returns
// privacy-transformed events; Close cancels the request and unblocks Next.
type QuerylogStream struct {
	// Node is the identity of the node that owns the stream.
	Node    string
	body    io.ReadCloser
	scanner *bufio.Scanner
	cancel  context.CancelFunc
	timer   *time.Timer
	once    sync.Once
}

// OpenQuerylogStream bounds establishment to two seconds, while an established
// stream lives until the caller cancels it. Idle peers are disconnected.
func (c *Client) OpenQuerylogStream(ctx context.Context, addr string, after uint64) (*QuerylogStream, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(2*time.Second, cancel)
	resp, err := c.get(streamCtx, addr, PathQuerylogStream+"?after="+strconv.FormatUint(after, 10))
	if err != nil {
		timer.Stop()
		cancel()
		return nil, err
	}
	if !timer.Stop() {
		_ = resp.Body.Close()
		cancel()
		return nil, context.DeadlineExceeded
	}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	idle := time.AfterFunc(45*time.Second, cancel)
	return &QuerylogStream{Node: resp.Header.Get(HeaderQuerylogNode), body: resp.Body, scanner: scanner, cancel: cancel, timer: idle}, nil
}

// Next reads one event from the stream.
func (s *QuerylogStream) Next() (*querylog.Event, error) {
	for s.scanner.Scan() {
		s.timer.Reset(45 * time.Second)
		line := s.scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var ev querylog.Event
		if err := json.Unmarshal([]byte(line[6:]), &ev); err != nil {
			return nil, err
		}
		return &ev, nil
	}
	if err := s.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}

// Close releases the peer connection.
func (s *QuerylogStream) Close() error {
	var err error
	s.once.Do(func() { s.timer.Stop(); s.cancel(); err = s.body.Close() })
	return err
}

// ReadLocalQuerylog dispatches a local read; it never contacts another node.
func ReadLocalQuerylog(ctx context.Context, r *sqlitelog.Reader, q QuerylogRequest) (*QuerylogResponse, error) {
	if r == nil {
		return nil, errors.New("query log is not stored")
	}
	out := &QuerylogResponse{}
	var err error
	top := sqlitelog.TopQuery{Window: q.Window, N: q.N, Blocked: q.Blocked, Candidates: true}
	switch q.Kind {
	case "summary":
		out.Summary, err = r.SummaryShard(ctx, sqlitelog.SummaryQuery{Window: q.Window})
	case "timeseries":
		out.Timeseries, err = r.Timeseries(ctx, sqlitelog.TimeseriesQuery{Window: q.Window, Step: q.Step})
	case "domains":
		out.Top, err = r.TopDomains(ctx, top)
	case "clients":
		out.Top, err = r.TopClients(ctx, top)
	case "lists":
		out.Top, err = r.TopLists(ctx, top)
	case "zones":
		out.Top, err = r.TopZones(ctx, top)
	case "upstreams":
		out.Upstreams, err = r.UpstreamShard(ctx, top)
	case "client_timeseries":
		out.Clients, err = r.ClientTimeseries(ctx, sqlitelog.ClientTimeseriesQuery{Window: q.Window, Step: q.Step, N: q.N, Candidates: true})
	case "recent_blocked":
		out.Blocked, err = r.RecentBlocked(ctx, q.N)
	case "queries":
		out.Queries, err = r.Queries(ctx, q.Filter)
	default:
		return nil, fmt.Errorf("unknown query-log operation %q", q.Kind)
	}
	return out, err
}
