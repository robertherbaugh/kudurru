package config

import (
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
)

// ManagedRootEdge declares one installed edge deployment and this node's exact
// advertised membership there. Addresses are host inputs, never peer redirects.
type ManagedRootEdge struct {
	Node                   string `koanf:"node" json:"node"`
	Address                string `koanf:"address" json:"address"`
	Deployment             string `koanf:"deployment" json:"deployment"`
	BackendAddress         string `koanf:"backendAddress" json:"backendAddress"`
	BackendInternalAddress string `koanf:"backendInternalAddress" json:"backendInternalAddress"`
}

func managedEdgeAddress(s string) bool {
	if len(s) > 512 {
		return false
	}
	host, port, err := net.SplitHostPort(s)
	n, portErr := strconv.Atoi(port)
	return err == nil && portErr == nil && host != "" && n > 0 && n <= 65535 && !strings.ContainsAny(host, "/?#@\\% \t\r\n")
}

func (c *Config) validateManagedRootEdges() error {
	invalid := errors.New("upstreams.recursion.managedEdges: edges requires SQLite, a node name and 1–32 unique complete edge deployments")
	fleet := c.Upstreams.Recursion.ManagedEdges
	if c.Store.Driver != "sqlite" || c.Node.Name == "" || len(c.Node.Name) > 256 || len(fleet) == 0 || len(fleet) > 32 {
		return invalid
	}
	nodes, addresses, deployments := map[string]bool{}, map[string]bool{}, map[string]bool{}
	for _, edge := range fleet {
		digest, err := hex.DecodeString(edge.Deployment)
		if edge.Node == "" || len(edge.Node) > 256 || !managedEdgeAddress(edge.Address) ||
			!managedEdgeAddress(edge.BackendAddress) || !managedEdgeAddress(edge.BackendInternalAddress) ||
			err != nil || len(digest) != 32 || edge.Deployment != strings.ToLower(edge.Deployment) || edge.Deployment == strings.Repeat("0", 64) ||
			nodes[edge.Node] || addresses[edge.Address] || deployments[edge.Deployment] {
			return invalid
		}
		nodes[edge.Node], addresses[edge.Address], deployments[edge.Deployment] = true, true, true
	}
	return nil
}
