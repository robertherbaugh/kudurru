package api

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/robertherbaugh/kudurru/internal/policy"
	"github.com/robertherbaugh/kudurru/internal/zonecache"
)

// NodeInfo is one node of GET /nodes.
type NodeInfo struct {
	Name    string `json:"name"`
	Address string `json:"address,omitempty"`
	Self    bool   `json:"self"`
	Leader  bool   `json:"leader"`
	// Ready is nil when the node's readiness is unknown.
	Ready    *bool    `json:"ready,omitempty"`
	Unready  []string `json:"unready,omitempty"`
	Version  string   `json:"version,omitempty"`
	State    string   `json:"state,omitempty" doc:"zonecache state: unsynced, fromSnapshot, live."`
	Snapshot string   `json:"snapshotAge,omitempty" doc:"Age of the served local.db snapshot (fromSnapshot only)."`
	Error    string   `json:"error,omitempty" doc:"Error fetching this node's status; readiness is unknown."`
}

type nodesOutput struct{ Body *List[NodeInfo] }

type nodeNameInput struct {
	Name string `path:"name"`
}

// NodeHealth is GET /nodes/{name}/health: the /healthz document of the
// node.
type NodeHealth struct {
	Node   string         `json:"node"`
	Ready  bool           `json:"ready"`
	Health map[string]any `json:"health"`
	Took
}

type nodeHealthOutput struct{ Body *NodeHealth }

// ClusterInfo is GET /cluster.
type ClusterInfo struct {
	ClusterID string `json:"clusterID,omitempty"`
	Driver    string `json:"driver"`
	Epoch     uint64 `json:"epoch"`
	Leader    string `json:"leader,omitempty"`
	Nodes     int    `json:"nodes"`
	// ReadOnly is true in the ha-static shape: objects are file-managed on
	// every node and the API refuses object writes (503 read_only).
	ReadOnly bool   `json:"readOnly"`
	Node     string `json:"node"`
	Took
}

type clusterOutput struct{ Body *ClusterInfo }

// EdgeInfo is GET /edge (dnsdist backends and VIP owners; the edge and
// VIP supervisors are not wired into the API yet).
type EdgeInfo struct {
	Edge     EdgeComponent `json:"edge"`
	VIP      EdgeComponent `json:"vip"`
	Backends []EdgeBackend `json:"backends" nullable:"false"`
	Node     string        `json:"node"`
	Took
}

// EdgeComponent is the state of the dnsdist or keepalived supervisor.
type EdgeComponent struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state,omitempty"`
}

// EdgeBackend is one dnsdist backend.
type EdgeBackend struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Up      bool   `json:"up"`
}

type edgeOutput struct{ Body *EdgeInfo }

// DiagnosisMessage is one entry of GET /info/messages: a configuration or
// runtime problem the UI badges (§10 item 8).
type DiagnosisMessage struct {
	Severity string `json:"severity" enum:"error,warning,info"`
	// Source names the producer: objects, policy, blocklists, zonecache,
	// health.
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Object  string `json:"object,omitempty"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
	Node    string `json:"node"`
}

type messagesOutput struct{ Body *List[DiagnosisMessage] }

func (s *Server) registerNodes() {
	cl := perm{resource: "cluster", verb: "read"}
	register(s, operation(http.MethodGet, "/nodes", "listNodes", "Nodes of the cluster", "cluster"), cl, s.listNodes)
	register(s, operation(http.MethodGet, "/nodes/{name}/health", "nodeHealth", "Readiness detail of one node", "cluster"), cl, s.nodeHealth)
	register(s, operation(http.MethodGet, "/cluster", "getCluster", "Store, leader and shape", "cluster"), cl, s.getCluster)
	register(s, operation(http.MethodGet, "/edge", "getEdge", "dnsdist backends and VIP owners", "cluster"), perm{resource: "edge", verb: "read"}, s.getEdge)
	register(s, operation(http.MethodGet, "/info/messages", "infoMessages", "Diagnosis messages (objects, policies, lists, readiness)", "cluster"),
		perm{resource: "settings", verb: "read"}, s.infoMessages)
}

// zoneState names the zonecache state.
func (s *Server) zoneState() (string, string) {
	if s.opts.Cache == nil {
		return "", ""
	}
	return s.opts.Cache.State().String(), s.opts.Cache.Snapshot().SnapshotAge(s.opts.Now())
}

func (s *Server) listNodes(ctx context.Context, _ *struct{}) (*nodesOutput, error) {
	peers, err := s.opts.Membership.Peers(ctx)
	if err != nil {
		return nil, errUnavailable("membership: %v", err)
	}
	leader, _ := s.opts.Membership.Leader(ctx)
	items := make([]NodeInfo, len(peers))
	var wg sync.WaitGroup
	for i, p := range peers {
		n := NodeInfo{Name: p.Name, Address: p.Address, Self: p.Self || p.Name == s.opts.NodeName, Leader: leader.Name == p.Name && leader.Name != ""}
		if n.Self {
			ready, unready := s.ready()
			n.Ready, n.Unready = &ready, unready
			n.Version = s.opts.Version.Version
			n.State, n.Snapshot = s.zoneState()
		}
		items[i] = n
		if !n.Self && s.opts.InternalClient != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				peerCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
				defer cancel()
				status, err := s.opts.InternalClient.Status(peerCtx, p.Address)
				if err != nil {
					items[i].Error = peerStatusError(err)
					s.log.WarnContext(ctx, "peer status unavailable", "node", p.Name, "address", p.Address, "error", err)
					return
				}
				items[i].Ready, items[i].Unready = status.Ready, status.Unready
				items[i].Version, items[i].State, items[i].Snapshot = status.Version, status.State, status.Snapshot
			}()
		}
	}
	wg.Wait()
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	return &nodesOutput{Body: newList(items, "")}, nil
}

func peerStatusError(err error) string {
	var netErr net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout():
		return "peer status: timeout"
	case errors.Is(err, syscall.ECONNREFUSED):
		return "peer status: connection refused"
	default:
		return "peer status: unavailable"
	}
}

func (s *Server) ready() (bool, []string) {
	if s.opts.Health == nil {
		return true, nil
	}
	return s.opts.Health.Ready()
}

func (s *Server) nodeHealth(ctx context.Context, in *nodeNameInput) (*nodeHealthOutput, error) {
	if in.Name != s.opts.NodeName {
		peers, _ := s.opts.Membership.Peers(ctx)
		for _, p := range peers {
			if p.Name == in.Name {
				return nil, newError(http.StatusNotImplemented, KeyNotImplemented, "health of other nodes needs scatter-gather over the internal listener",
					"ask "+in.Name+" directly")
			}
		}
		return nil, errNotFound("node " + in.Name)
	}
	ready, _ := s.ready()
	out := &NodeHealth{Node: s.opts.NodeName, Ready: ready, Health: map[string]any{}}
	if s.opts.Health != nil {
		rec := httptest.NewRecorder()
		s.opts.Health.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(ctx, http.MethodGet, "/healthz", nil))
		_ = json.Unmarshal(rec.Body.Bytes(), &out.Health)
	}
	return &nodeHealthOutput{Body: out}, nil
}

func (s *Server) getCluster(ctx context.Context, _ *struct{}) (*clusterOutput, error) {
	info := s.opts.Store.Info()
	out := &ClusterInfo{ClusterID: info.ClusterID, Driver: info.Driver, Epoch: info.Epoch, Leader: info.Leader, Node: s.opts.NodeName, Nodes: 1}
	if l, err := s.opts.Membership.Leader(ctx); err == nil && l.Name != "" {
		out.Leader = l.Name
	}
	if peers, err := s.opts.Membership.Peers(ctx); err == nil {
		out.Nodes = len(peers)
	}
	if s.opts.Objects != nil {
		out.ReadOnly = s.opts.Objects.APIReadOnly()
	}
	return &clusterOutput{Body: out}, nil
}

func (s *Server) getEdge(context.Context, *struct{}) (*edgeOutput, error) {
	return &edgeOutput{Body: &EdgeInfo{Backends: []EdgeBackend{}, Node: s.opts.NodeName}}, nil
}

func (s *Server) infoMessages(context.Context, *struct{}) (*messagesOutput, error) {
	var out []DiagnosisMessage
	add := func(m DiagnosisMessage) {
		m.Node = s.opts.NodeName
		out = append(out, m)
	}
	if s.opts.Objects != nil {
		for _, d := range s.opts.Objects.Diagnoses() {
			src := d.Source
			if d.Document > 0 {
				src = d.String()
				src = src[:strings.Index(src, ":")]
			}
			add(DiagnosisMessage{Severity: string(d.Severity), Source: "objects", Reason: string(d.Reason), Object: d.Object,
				Message: src + ": " + d.Message, Hint: d.Hint})
		}
	}
	if s.opts.Policy != nil {
		if ix := s.opts.Policy.Index(); ix != nil {
			for _, d := range ix.Diagnoses() {
				sev := "warning"
				if d.Reason == policy.ReasonDisabled {
					sev = "info" // a reference to an object switched off on purpose
				}
				add(DiagnosisMessage{Severity: sev, Source: "policy", Reason: d.Reason, Object: d.Kind + "/" + d.Object, Message: d.Message})
			}
		}
	}
	if s.opts.Lists != nil {
		if ok, msgs := s.opts.Lists.Ready(); !ok || len(msgs) > 0 {
			sev := "warning"
			if !ok {
				sev = "error"
			}
			for _, m := range msgs {
				add(DiagnosisMessage{Severity: sev, Source: "blocklists", Reason: "ListsNotLoaded", Message: m})
			}
		}
	}
	if s.opts.Cache != nil {
		for _, c := range s.opts.Cache.Degraded() {
			add(DiagnosisMessage{Severity: "warning", Source: "zonecache", Reason: c.Reason, Object: c.Builder, Message: c.String()})
		}
		if snap := s.opts.Cache.Snapshot(); snap != nil && snap.State == zonecache.FromSnapshot {
			msg := "serving from the local.db snapshot while the store is unreachable"
			if snap.Stale {
				msg = "serving a stale local.db snapshot; names in primary zones answer SERVFAIL"
			}
			add(DiagnosisMessage{Severity: "warning", Source: "zonecache", Reason: "ServingFromSnapshot", Message: msg})
		}
	}
	if s.opts.Objects != nil && s.opts.Objects.APIReadOnly() {
		add(DiagnosisMessage{Severity: "info", Source: "cluster", Reason: "ReadOnly", Message: "objects are read-only through the API (ha-static)",
			Hint: "edit objects.d on every node"})
	}
	if ok, unready := s.ready(); !ok {
		for _, u := range unready {
			add(DiagnosisMessage{Severity: "error", Source: "health", Reason: "NotReady", Message: u})
		}
	}
	return &messagesOutput{Body: newList(out, "")}, nil
}
