package keepalived

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/netip"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/robertherbaugh/kudurru/internal/edge"
)

// PriorityIndexLabel on a Node pins its priority index (0 = preferred
// owner of the first VIP) instead of its position among the selected
// Nodes sorted by name.
const PriorityIndexLabel = "kudurru.io/vip-priority-index"

// ParsePeersFrom parses `--peers-from nodes:<label selector>` and returns
// the selector ("" selects every Node).
func ParsePeersFrom(s string) (string, error) {
	sel, ok := strings.CutPrefix(s, "nodes:")
	if !ok {
		return "", fmt.Errorf("keepalived: --peers-from %q: want nodes:<label selector>", s)
	}
	return sel, nil
}

// NodePeers derives keepalived's unicast peers from the Kubernetes Nodes
// that run the edge DaemonSet (§5.4, `--peers-from nodes:<selector>`):
// every selected Node except this one contributes its InternalIP (else
// ExternalIP) in the VIPs' address family. The ServiceAccount needs
// get/list/watch on nodes (the chart's <release>-edge-nodes ClusterRole).
type NodePeers struct {
	Client corev1client.NodesGetter
	// Selector is a label selector; empty selects every Node.
	Selector string
	// Self is this node's name (the pod's spec.nodeName).
	Self string
	// IPv6 selects the address family of the VIPs.
	IPv6 bool
	// Interval is the re-list period of Sync; default 30 s.
	Interval time.Duration
}

// NodeSet is the peers and this node's place among the selected Nodes.
type NodeSet struct {
	// Peers are the other nodes' addresses, sorted by node name.
	Peers []string
	// Index is this node's priority index: the PriorityIndexLabel value
	// when set, else its position among the selected Nodes by name.
	Index int
	// Count is the number of selected Nodes, this one included.
	Count int
}

// List reads the selected Nodes once.
func (n *NodePeers) List(ctx context.Context) (NodeSet, error) {
	list, err := n.Client.Nodes().List(ctx, metav1.ListOptions{LabelSelector: n.Selector})
	if err != nil {
		return NodeSet{}, fmt.Errorf("keepalived: list nodes (%q): %w", n.Selector, err)
	}
	nodes := list.Items
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Name < nodes[j].Name })
	set := NodeSet{Index: -1, Count: len(nodes)}
	for i := range nodes {
		node := &nodes[i]
		if node.Name == n.Self {
			set.Index = i
			if v, ok := node.Labels[PriorityIndexLabel]; ok {
				idx, err := strconv.Atoi(v)
				if err != nil || idx < 0 {
					return NodeSet{}, fmt.Errorf("keepalived: node %s: label %s=%q is not a priority index", node.Name, PriorityIndexLabel, v)
				}
				set.Index = idx
			}
			continue
		}
		addr, ok := nodeAddress(node, n.IPv6)
		if !ok {
			continue // no address in the family yet (node still joining)
		}
		set.Peers = append(set.Peers, addr)
	}
	if set.Index < 0 {
		return NodeSet{}, fmt.Errorf("keepalived: node %q is not among the nodes matching %q", n.Self, n.Selector)
	}
	return set, nil
}

// nodeAddress returns the InternalIP (else ExternalIP) of the family.
func nodeAddress(node *corev1.Node, ipv6 bool) (string, bool) {
	for _, kind := range []corev1.NodeAddressType{corev1.NodeInternalIP, corev1.NodeExternalIP} {
		for _, a := range node.Status.Addresses {
			if a.Type != kind {
				continue
			}
			ip, err := netip.ParseAddr(a.Address)
			if err != nil || ip.Is4() == ipv6 {
				continue
			}
			return ip.String(), true
		}
	}
	return "", false
}

// WritePeersFile writes peers one per line (ReadPeersFile's format).
func WritePeersFile(path string, peers []string) error {
	var b strings.Builder
	b.WriteString("# unicast peers from the Kubernetes Nodes (kudurru vip run --peers-from)\n")
	for _, p := range peers {
		b.WriteString(p)
		b.WriteByte('\n')
	}
	return edge.WriteFileAtomic(path, []byte(b.String()), 0o644) //nolint:gosec // addresses, not secrets
}

// Sync re-lists the Nodes every Interval and rewrites path when the peer
// set changes; the supervisor watches the file (PeersFile) and re-renders
// keepalived.conf. The priority index stays what it was at start: a
// re-render must never move this node's priority under a running VIP.
// It returns when ctx ends.
func (n *NodePeers) Sync(ctx context.Context, path string, initial []string, log *slog.Logger) {
	if log == nil {
		log = slog.Default()
	}
	interval := n.Interval
	if interval <= 0 {
		interval = 30 * time.Second
	}
	last := slices.Clone(initial)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		set, err := n.List(ctx)
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				log.Warn("node peers: keeping the previous peers", "err", err)
			}
			continue
		}
		if slices.Equal(set.Peers, last) {
			continue
		}
		if len(set.Peers) == 0 {
			log.Warn("node peers: no other node matches; keeping the previous peers", "selector", n.Selector)
			continue
		}
		if err := WritePeersFile(path, set.Peers); err != nil {
			log.Error("node peers: write peers file", "file", path, "err", err)
			continue
		}
		log.Info("node peers changed", "peers", set.Peers, "nodes", set.Count)
		last = set.Peers
	}
}
