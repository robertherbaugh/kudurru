package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/robertherbaugh/kudurru/ee/edge/keepalived"
)

// nodesClient builds the Nodes client of `--peers-from nodes:<selector>`
// (in-cluster config or KUBECONFIG); tests replace it.
var nodesClient = func() (corev1client.NodesGetter, error) {
	rc, err := ctrlconfig.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("kubernetes client: %w", err)
	}
	return corev1client.NewForConfig(rc)
}

// nodePeers resolves --peers-from: it lists the selected Nodes, sets
// f.peers and, unless --priority-index was given, f.priorityIndex from
// this node's place among them. With writeFile (vip run) the peers go to
// <run dir>/vrrp-peers, which becomes the watched --peers-file. It
// returns nil when --peers-from is unset.
func (f *vipFlags) nodePeers(c *cobra.Command, writeFile bool) (*keepalived.NodePeers, []string, error) {
	if f.peersFrom == "" {
		return nil, nil, nil
	}
	if len(f.peers) > 0 || f.peersFile != "" {
		return nil, nil, errors.New("--peers-from cannot be combined with --peers or --peers-file")
	}
	sel, err := keepalived.ParsePeersFrom(f.peersFrom)
	if err != nil {
		return nil, nil, err
	}
	if len(f.vips) == 0 {
		return nil, nil, errors.New("at least one --vip is required")
	}
	v, err := keepalived.ParseVIP(f.vips[0])
	if err != nil {
		return nil, nil, err
	}
	client, err := nodesClient()
	if err != nil {
		return nil, nil, err
	}
	np := &keepalived.NodePeers{Client: client, Selector: sel, Self: nodeName(f.routerID), IPv6: v.Prefix.Addr().Is6()}
	set, err := np.List(c.Context())
	if err != nil {
		return nil, nil, err
	}
	if len(set.Peers) == 0 {
		return nil, nil, fmt.Errorf("--peers-from %s: no other node with an address of the VIP's family matches", f.peersFrom)
	}
	if !c.Flags().Changed("priority-index") {
		f.priorityIndex = set.Index
	}
	if writeFile {
		if err := os.MkdirAll(f.runDir, 0o755); err != nil { //nolint:gosec // the run volume is shared with kudurru and dnsdist
			return nil, nil, err
		}
		f.peersFile = filepath.Join(f.runDir, "vrrp-peers")
		if err := keepalived.WritePeersFile(f.peersFile, set.Peers); err != nil {
			return nil, nil, err
		}
	} else {
		f.peers = set.Peers
	}
	return np, set.Peers, nil
}
