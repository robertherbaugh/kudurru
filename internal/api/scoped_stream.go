package api

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humago"
	"github.com/robertherbaugh/kudurru/internal/cluster"
	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/querylog"
	"github.com/robertherbaugh/kudurru/internal/querylog/sqlitelog"
)

type streamCursor struct {
	Version   int               `json:"v"`
	Positions map[string]uint64 `json:"positions"`
	Filter    string            `json:"filter"`
}
type streamSource struct {
	peer   cluster.Peer
	events <-chan *querylog.Event
	remote *internalapi.QuerylogStream
	err    error
}
type streamItem struct {
	index int
	event *querylog.Event
	err   error
	ready bool
}

func (s *Server) openStreamSource(ctx context.Context, p cluster.Peer, after uint64) streamSource {
	source := streamSource{peer: p}
	if p.Self {
		if s.opts.Tail != nil {
			source.events = s.opts.Tail.Subscribe(ctx)
		} else if s.localQuerylog() != nil {
			source.events = s.localQuerylog().Subscribe(ctx)
		} else {
			source.err = fmt.Errorf("live tail unavailable")
		}
	} else if s.opts.InternalClient == nil || p.Address == "" {
		source.err = fmt.Errorf("peer transport unavailable")
	} else {
		source.remote, source.err = s.opts.InternalClient.OpenQuerylogStream(ctx, p.Address, after)
		if source.err == nil && source.remote.Node != p.Name {
			_ = source.remote.Close()
			source.remote = nil
			source.err = fmt.Errorf("peer stream identity does not match requested node")
		}
	}
	return source
}

func (s *Server) scopedQueryStream(ctx context.Context, in *streamInput, peers []cluster.Peer) (*huma.StreamResponse, error) {
	filter, err := newStreamFilter(in.QueryParams)
	if err != nil {
		return nil, err
	}
	params := in.QueryParams
	params.Scope = ""
	params.Node = ""
	params.Cursor = ""
	params.Limit = 0
	filterJSON, _ := json.Marshal(params)
	cursor := streamCursor{Version: 1, Positions: map[string]uint64{}, Filter: string(filterJSON)}
	raw := in.LastEventID
	if raw == "" {
		raw = in.LastEventIDQuery
	}
	clusterScope := in.Scope == "cluster"
	if raw != "" {
		if clusterScope {
			if err := decodeQuerylogCursor(raw, &cursor); err != nil {
				return nil, err
			}
			if cursor.Version != 1 || cursor.Positions == nil || cursor.Filter != string(filterJSON) {
				return nil, errInvalid("stream cursor does not match these filters")
			}
		} else {
			after, err := strconv.ParseUint(raw, 10, 64)
			if err != nil {
				return nil, errInvalid("invalid stream cursor")
			}
			cursor.Positions[peers[0].Name] = after
		}
	}
	positions := make(map[string]uint64, len(peers))
	for _, p := range peers {
		positions[p.Name] = cursor.Positions[p.Name]
	}
	cursor.Positions = positions
	select {
	case s.streams <- struct{}{}:
	default:
		return nil, errUnavailable("too many live query streams")
	}
	sctx, cancel := context.WithCancel(ctx)
	sources := make([]streamSource, len(peers))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	for i, p := range peers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-sctx.Done():
				sources[i] = streamSource{peer: p, err: sctx.Err()}
				return
			}
			sources[i] = s.openStreamSource(sctx, p, cursor.Positions[p.Name])
		}()
	}
	wg.Wait()
	scope := sqlitelog.Scope{Scope: "node", Nodes: make([]sqlitelog.NodeResult, len(peers))}
	if clusterScope {
		scope.Scope = "cluster"
	}
	available := 0
	for i, source := range sources {
		nr := sqlitelog.NodeResult{Name: source.peer.Name, OK: source.err == nil}
		if source.err != nil {
			nr.Error = "live tail: unavailable"
			scope.Partial = true
			s.log.Warn("peer live tail failed", "node", source.peer.Name, "err", source.err)
		} else {
			available++
		}
		scope.Nodes[i] = nr
	}
	if available == 0 {
		cancel()
		<-s.streams
		return nil, errUnavailable("live tail unavailable on the requested nodes")
	}
	return &huma.StreamResponse{Body: func(hctx huma.Context) {
		defer func() {
			cancel()
			for _, source := range sources {
				if source.remote != nil {
					_ = source.remote.Close()
				}
			}
			<-s.streams
		}()
		_, w := humago.Unwrap(hctx)
		h := w.Header()
		h.Set("Content-Type", "text/event-stream")
		h.Set("Cache-Control", "no-cache")
		h.Set("X-Accel-Buffering", "no")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		arm := func() { _ = rc.SetWriteDeadline(time.Now().Add(sseWriteTimeout)) }
		arm()
		items := make(chan streamItem, 256)
		sendItem := func(item streamItem) bool {
			select {
			case items <- item:
				return true
			case <-sctx.Done():
				return false
			}
		}
		for i, source := range sources {
			after := cursor.Positions[source.peer.Name]
			go func() {
				if source.peer.Self {
					if source.err != nil {
						sendItem(streamItem{index: i, err: source.err})
						return
					}
					if after != 0 && s.opts.Tail != nil {
						for _, ev := range s.opts.Tail.Recent(0, after) {
							if !sendItem(streamItem{index: i, event: ev}) {
								return
							}
						}
					}
					for {
						select {
						case <-sctx.Done():
							return
						case ev, ok := <-source.events:
							if !ok {
								sendItem(streamItem{index: i, err: fmt.Errorf("local tail closed")})
								return
							}
							if !sendItem(streamItem{index: i, event: ev}) {
								return
							}
						}
					}
				}
				remote := source.remote
				wasOK := source.err == nil
				setReady := func(err error) bool {
					ready := err == nil
					if ready == wasOK {
						return true
					}
					if !sendItem(streamItem{index: i, err: err, ready: ready}) {
						return false
					}
					wasOK = ready
					return true
				}
				backoff := 3 * time.Second
				for {
					if remote != nil {
						for {
							ev, err := remote.Next()
							if err != nil {
								_ = remote.Close()
								remote = nil
								if !setReady(err) {
									return
								}
								break
							}
							after = max(after, ev.ID)
							if !sendItem(streamItem{index: i, event: ev}) {
								_ = remote.Close()
								return
							}
						}
					}
					// Equal jitter spreads dashboard retries while keeping the
					// exponential delay bounded at 30 seconds.
					delay := backoff/2 + time.Duration(rand.Int64N(int64(backoff/2))) //nolint:gosec // retry jitter, not a secret
					timer := time.NewTimer(delay)
					select {
					case <-sctx.Done():
						timer.Stop()
						return
					case <-timer.C:
					}
					backoff = min(2*backoff, 30*time.Second)
					if s.opts.InternalClient == nil || source.peer.Address == "" {
						continue
					}
					var err error
					remote, err = s.opts.InternalClient.OpenQuerylogStream(sctx, source.peer.Address, after)
					if err == nil && remote.Node != source.peer.Name {
						_ = remote.Close()
						remote = nil
						err = fmt.Errorf("peer stream identity does not match requested node")
					}
					if err != nil {
						if !setReady(err) {
							return
						}
						continue
					}
					backoff = 3 * time.Second
					if !setReady(nil) {
						_ = remote.Close()
						return
					}
				}
			}()
		}
		writeScope := func() bool {
			scope.Partial = false
			for _, nr := range scope.Nodes {
				scope.Partial = scope.Partial || !nr.OK
			}
			b, _ := json.Marshal(scope)
			arm()
			_, err := fmt.Fprintf(w, "event: scope\ndata: %s\n\n", b)
			return err == nil && rc.Flush() == nil
		}
		if !writeScope() {
			return
		}
		tick := time.NewTicker(sseHeartbeat)
		defer tick.Stop()
		for {
			select {
			case <-sctx.Done():
				return
			case item := <-items:
				if item.event == nil {
					nr := &scope.Nodes[item.index]
					if nr.OK == item.ready {
						continue
					}
					nr.OK = item.ready
					nr.Error = ""
					if !item.ready {
						nr.Error = "live tail: unavailable"
					}
					if !writeScope() {
						return
					}
					if !clusterScope && !item.ready {
						return
					}
					continue
				}
				ev := item.event
				node := sources[item.index].peer.Name
				if ev.ID != 0 && ev.ID <= cursor.Positions[node] {
					continue
				}
				cursor.Positions[node] = max(cursor.Positions[node], ev.ID)
				if !filter.match(ev) {
					continue
				}
				row := eventRow(ev)
				row.Node = node
				b, err := json.Marshal(row)
				if err != nil {
					continue
				}
				id := strconv.FormatUint(ev.ID, 10)
				if clusterScope {
					id = encodeQuerylogCursor(cursor)
				}
				arm()
				if _, err := fmt.Fprintf(w, "id: %s\nevent: query\ndata: %s\n\n", id, b); err != nil || rc.Flush() != nil {
					return
				}
			case <-tick.C:
				// A rolling deployment changes pod names or addresses. End
				// this stream so EventSource reconnects against fresh membership
				// using its per-node resume positions.
				peerCtx, stop := context.WithTimeout(sctx, 2*time.Second)
				current, err := s.queryPeers(peerCtx, in.Scope, in.Node)
				stop()
				if err == nil && !sameQueryPeers(peers, current) {
					return
				}
				arm()
				if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
					return
				}
			}
		}
	}}, nil
}

func sameQueryPeers(a, b []cluster.Peer) bool {
	if len(a) != len(b) {
		return false
	}
	for i, p := range a {
		if p.Name != b[i].Name || p.Address != b[i].Address {
			return false
		}
	}
	return true
}
