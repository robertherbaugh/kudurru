package config

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// ClusterConfig describes host-selected membership, never replicated authority.
// Auto retains the store driver's election; static nodes each lead locally.
type ClusterConfig struct {
	Role  string       `koanf:"role" json:"role"`
	Peers []StaticPeer `koanf:"peers" json:"peers"`
}

// StaticPeer is a named internal TLS endpoint. It grants no writer authority.
type StaticPeer struct {
	Name    string `koanf:"name" json:"name"`
	Address string `koanf:"address" json:"address"`
}

// Static reports the explicit file-managed SQLite twin mode.
func (c ClusterConfig) Static() bool { return c.Role == "static" }

func (c *Config) validateStaticCluster() []error {
	if !c.Cluster.Static() {
		if len(c.Cluster.Peers) != 0 {
			return []error{errors.New("cluster.peers: requires cluster.role static")}
		}
		return nil
	}
	var errs []error
	if c.Store.Driver != "sqlite" {
		errs = append(errs, errors.New("cluster.role: static requires store.driver sqlite"))
	}
	if c.ExternalDNS.Webhook.Enabled {
		errs = append(errs, errors.New("cluster.role: static refuses the external-dns mutation webhook; distribute object files to every node"))
	}
	if len(c.Cluster.Peers) == 0 || len(c.Cluster.Peers) > 64 {
		errs = append(errs, errors.New("cluster.peers: static requires between 1 and 64 named endpoints including this node"))
	}
	names, addresses := map[string]bool{}, map[string]bool{}
	for i, p := range c.Cluster.Peers {
		path := fmt.Sprintf("cluster.peers[%d]", i)
		if len(validation.IsDNS1123Subdomain(p.Name)) != 0 || names[p.Name] {
			errs = append(errs, fmt.Errorf("%s.name: use a unique lowercase DNS name", path))
		}
		names[p.Name] = true
		host, port, err := net.SplitHostPort(p.Address)
		n, portErr := strconv.Atoi(port)
		address := ""
		switch ip, ipErr := netip.ParseAddr(host); {
		case ipErr == nil:
			if !ip.IsUnspecified() && !ip.IsMulticast() && ip.Zone() == "" {
				address = net.JoinHostPort(ip.Unmap().String(), strconv.Itoa(n))
			}
		case len(validation.IsDNS1123Subdomain(host)) == 0:
			address = net.JoinHostPort(host, strconv.Itoa(n))
		}
		if err != nil || portErr != nil || n < 1 || n > 65535 || address == "" || strings.TrimSpace(p.Address) != p.Address || addresses[address] {
			errs = append(errs, fmt.Errorf("%s.address: use a unique unicast IP or lowercase DNS host and port (1-65535)", path))
		}
		addresses[address] = true
	}
	if !names[c.Node.Name] {
		errs = append(errs, errors.New("cluster.peers: must include this node.name exactly once"))
	}
	return errs
}
