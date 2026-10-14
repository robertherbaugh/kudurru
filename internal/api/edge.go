package api

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// EdgeSupervisorEndpoint is a discovered edge pod or a local supervisor.
// URLs name the supervisor health listener, never dnsdist's private API.
type EdgeSupervisorEndpoint struct {
	Node, EdgeURL, VIPURL string
}

// EdgeSupervisorStatus retains failures alongside healthy supervisors.
type EdgeSupervisorStatus struct {
	Node string        `json:"node"`
	Edge EdgeComponent `json:"edge"`
	VIP  EdgeComponent `json:"vip"`
}

// EdgeVIP is the last observed VRRP state on one node. MASTER identifies
// an owner; BACKUP does not imply knowledge of the current owner.
type EdgeVIP struct {
	Name     string `json:"name"`
	Address  string `json:"address,omitempty"`
	Node     string `json:"node"`
	State    string `json:"state"`
	Priority int    `json:"priority"`
}

type supervisorDocument struct {
	Ready   *bool  `json:"ready"`
	Message string `json:"message"`
	Node    string `json:"node"`
	DNSDist *struct {
		Servers []struct{ Name, Address, State string } `json:"servers"`
	} `json:"dnsdist"`
	Addresses map[string]string `json:"addresses"`
	State     struct {
		Node string `json:"node"`
		VIPs map[string]struct {
			State    string `json:"state"`
			Priority int    `json:"priority"`
		} `json:"vips"`
	} `json:"state"`
}

var supervisorHTTP = &http.Client{
	Timeout:       2 * time.Second,
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

func supervisorGet(ctx context.Context, base, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	return supervisorHTTP.Do(req)
}

func readSupervisor(ctx context.Context, base string) (EdgeComponent, supervisorDocument) {
	c := EdgeComponent{Enabled: base != "", State: "unavailable"}
	var doc supervisorDocument
	if base == "" {
		return EdgeComponent{}, doc
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := supervisorGet(ctx, base, "/status")
	if err != nil {
		c.Message = strings.Replace(peerStatusError(err), "peer status:", "supervisor status:", 1)
		return c, doc
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		c.Message = fmt.Sprintf("supervisor status: HTTP %d", resp.StatusCode)
		return c, doc
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&doc); err != nil {
		c.Message = "invalid supervisor status"
		return c, supervisorDocument{}
	}
	// RC.4 status documents lack readiness. Read the health endpoint during
	// rolling upgrades instead of mistaking a responsive supervisor for DNS.
	if doc.Ready == nil {
		health, err := supervisorGet(ctx, base, "/healthz/ready")
		if err != nil {
			c.Message = strings.Replace(peerStatusError(err), "peer status:", "supervisor readiness:", 1)
			return c, doc
		}
		defer func() { _ = health.Body.Close() }()
		if health.StatusCode != http.StatusOK && health.StatusCode != http.StatusServiceUnavailable {
			c.Message = "invalid supervisor readiness"
			return c, doc
		}
		ready := health.StatusCode == http.StatusOK
		doc.Ready = &ready
	}
	c.State, c.Message = "down", doc.Message
	if *doc.Ready {
		c.State = "running"
	}
	return c, doc
}

func aggregateSupervisors(items []EdgeSupervisorStatus, vip bool) EdgeComponent {
	result := EdgeComponent{}
	anyRunning, anyNotRunning := false, false
	// Preserve partial VIP health, but prefer a known failure over an
	// unreachable supervisor when no supervisor is fully ready.
	severity := map[string]int{"running": 0, "degraded": 1, "unavailable": 2, "down": 3}
	for _, item := range items {
		c := item.Edge
		if vip {
			c = item.VIP
		}
		if !c.Enabled {
			continue
		}
		if c.State == "running" {
			anyRunning = true
		} else {
			anyNotRunning = true
		}
		if !result.Enabled || severity[c.State] > severity[result.State] {
			result = c
		}
	}
	if anyRunning && anyNotRunning {
		result.State, result.Message = "degraded", "some supervisors are not ready"
	}
	return result
}

func (s *Server) getEdge(ctx context.Context, _ *struct{}) (*edgeOutput, error) {
	out := &EdgeInfo{Node: s.opts.NodeName, Backends: []EdgeBackend{}, VIPs: []EdgeVIP{}, Supervisors: []EdgeSupervisorStatus{}}
	if s.opts.EdgeSupervisors == nil {
		return &edgeOutput{Body: out}, nil
	}
	out.Edge = EdgeComponent{Enabled: true, State: "unavailable", Message: "no supervisors discovered"}
	if s.opts.VIPSupervised {
		out.VIP = out.Edge
	}
	discoverCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	eps, err := s.opts.EdgeSupervisors(discoverCtx)
	cancel()
	if err != nil {
		s.log.WarnContext(ctx, "edge discovery unavailable", "error", err)
		out.Edge.Message = "supervisor discovery unavailable"
		if out.VIP.Enabled {
			out.VIP.Message = out.Edge.Message
		}
		return &edgeOutput{Body: out}, nil
	}
	if len(eps) == 0 {
		return &edgeOutput{Body: out}, nil
	}
	out.Supervisors = make([]EdgeSupervisorStatus, len(eps))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, ep := range eps {
		wg.Go(func() {
			var edge, vip EdgeComponent
			var doc, vdoc supervisorDocument
			var requests sync.WaitGroup
			requests.Go(func() { edge, doc = readSupervisor(ctx, ep.EdgeURL) })
			requests.Go(func() { vip, vdoc = readSupervisor(ctx, ep.VIPURL) })
			requests.Wait()
			node := ep.Node
			if vdoc.Node != "" {
				node = vdoc.Node
			} else if vdoc.State.Node != "" {
				node = vdoc.State.Node
			}
			backends := []EdgeBackend{}
			seen := map[string]bool{}
			if doc.DNSDist != nil {
				for _, b := range doc.DNSDist.Servers {
					// Resolver/authoritative pools may repeat a backend.
					key := b.Name + "\x00" + b.Address
					if seen[key] {
						continue
					}
					seen[key] = true
					backends = append(backends, EdgeBackend{Name: b.Name, Address: b.Address, Node: node, Up: edge.State == "running" && strings.EqualFold(b.State, "up")})
				}
			}
			vips := []EdgeVIP{}
			vipReady := vip.State == "running"
			for name := range vdoc.State.VIPs {
				if vdoc.Addresses == nil {
					vdoc.Addresses = map[string]string{}
				}
				if _, ok := vdoc.Addresses[name]; !ok {
					vdoc.Addresses[name] = ""
				}
			}
			for name, addr := range vdoc.Addresses {
				v := vdoc.State.VIPs[name]
				state := v.State
				if !vipReady || state == "" {
					state = "unknown"
				}
				if state == "FAULT" || state == "STOP" {
					vip.State = "degraded"
					vip.Message = "one or more VIPs are in FAULT or STOP"
				}
				vips = append(vips, EdgeVIP{Name: name, Address: addr, Node: node, State: state, Priority: v.Priority})
			}
			out.Supervisors[i] = EdgeSupervisorStatus{Node: node, Edge: edge, VIP: vip}
			mu.Lock()
			out.Backends = append(out.Backends, backends...)
			out.VIPs = append(out.VIPs, vips...)
			mu.Unlock()
		})
	}
	wg.Wait()
	out.Edge = aggregateSupervisors(out.Supervisors, false)
	out.VIP = aggregateSupervisors(out.Supervisors, true)
	sort.Slice(out.Supervisors, func(i, j int) bool { return out.Supervisors[i].Node < out.Supervisors[j].Node })
	sort.Slice(out.Backends, func(i, j int) bool {
		a, b := out.Backends[i], out.Backends[j]
		if a.Node != b.Node {
			return a.Node < b.Node
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Address < b.Address
	})
	sort.Slice(out.VIPs, func(i, j int) bool {
		a, b := out.VIPs[i], out.VIPs[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return a.Node < b.Node
	})
	return &edgeOutput{Body: out}, nil
}
