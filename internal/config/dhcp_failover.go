package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

// DHCPFailover configures a three-voter, quorum-backed DHCPv4 allocator.
// TLS paths refer to files on this node; secret contents never enter the API.
type DHCPFailover struct {
	Enabled       bool               `koanf:"enabled" json:"enabled"`
	ClusterID     string             `koanf:"clusterID" json:"clusterID"`
	NodeID        string             `koanf:"nodeID" json:"nodeID"`
	ListenAddress string             `koanf:"listenAddress" json:"listenAddress"`
	Peers         []DHCPFailoverPeer `koanf:"peers" json:"peers"`
	Bootstrap     bool               `koanf:"bootstrap" json:"bootstrap"`
	CAFile        string             `koanf:"caFile" json:"caFile"`
	CertFile      string             `koanf:"certFile" json:"certFile"`
	KeyFile       string             `koanf:"keyFile" json:"keyFile"`
}

// DHCPFailoverPeer identifies a voter and an optional dashboard URL.
type DHCPFailoverPeer struct {
	ID      string `koanf:"id" json:"id"`
	Address string `koanf:"address" json:"address"`
	APIURL  string `koanf:"apiURL" json:"apiURL,omitempty"`
}

// Clone returns a detached value suitable for a saved setup snapshot.
func (f DHCPFailover) Clone() DHCPFailover { f.Peers = slices.Clone(f.Peers); return f }

func dhcpMemberName(value string) bool {
	if len(value) == 0 || len(value) > 63 {
		return false
	}
	for i, ch := range value {
		if ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-' && i > 0 && i < len(value)-1 {
			continue
		}
		return false
	}
	return true
}

func validateDHCPPeerAddress(value string, listen bool) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return fmt.Errorf("use an IP address or hostname and port: %w", err)
	}
	n, err := strconv.ParseUint(port, 10, 16)
	if err != nil || n == 0 {
		return fmt.Errorf("port must be 1..65535")
	}
	if host == "" && !listen {
		return fmt.Errorf("peer host is required")
	}
	if strings.ContainsAny(host, " /\\\t\r\n") {
		return fmt.Errorf("invalid host")
	}
	if ip, err := netip.ParseAddr(host); err == nil && (ip.IsMulticast() || ip.IsUnspecified() && !listen) {
		return fmt.Errorf("peer address must be unicast")
	}
	return nil
}

// Validate performs structural checks only; startup verifies credentials and
// persisted cluster identity before opening the peer listener.
func (f DHCPFailover) Validate() error {
	if !f.Enabled {
		return nil
	}
	if !dhcpMemberName(f.ClusterID) || !dhcpMemberName(f.NodeID) {
		return fmt.Errorf("dhcp.failover: clusterID and nodeID must be lowercase DNS labels")
	}
	if len(f.Peers) != 3 {
		return fmt.Errorf("dhcp.failover: exactly three voting peers are required")
	}
	if err := validateDHCPPeerAddress(f.ListenAddress, true); err != nil {
		return fmt.Errorf("dhcp.failover.listenAddress: %w", err)
	}
	ids, addresses := map[string]bool{}, map[string]bool{}
	for _, peer := range f.Peers {
		if !dhcpMemberName(peer.ID) || ids[peer.ID] || addresses[peer.Address] {
			return fmt.Errorf("dhcp.failover: peer IDs and addresses must be valid and unique")
		}
		if err := validateDHCPPeerAddress(peer.Address, false); err != nil {
			return fmt.Errorf("dhcp.failover peer %s: %w", peer.ID, err)
		}
		if peer.APIURL != "" {
			u, err := url.Parse(peer.APIURL)
			if err != nil || u.Scheme != "https" && u.Scheme != "http" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
				return fmt.Errorf("dhcp.failover peer %s: apiURL must be an HTTP(S) dashboard URL without credentials, query, or fragment", peer.ID)
			}
		}
		ids[peer.ID], addresses[peer.Address] = true, true
	}
	if !ids[f.NodeID] {
		return fmt.Errorf("dhcp.failover: nodeID must identify one of the three peers")
	}
	first := f.Peers[0].ID
	for _, peer := range f.Peers {
		if peer.ID < first {
			first = peer.ID
		}
	}
	if f.Bootstrap && f.NodeID != first {
		return fmt.Errorf("dhcp.failover: only the alphabetically first peer may bootstrap a new cluster")
	}
	if (f.CAFile != "" || f.CertFile != "" || f.KeyFile != "") && (f.CAFile == "" || f.CertFile == "" || f.KeyFile == "") {
		return fmt.Errorf("dhcp.failover: provide caFile, certFile, and keyFile together, or use the cluster encryption key")
	}
	return nil
}

// ValidateScopes verifies that every replica reserves every voter's server
// address and that the local listener uses this node's configured address.
func (f DHCPFailover) ValidateScopes(scopes []dhcp.Config) error {
	if err := f.Validate(); err != nil {
		return err
	}
	if !f.Enabled {
		return nil
	}
	for _, scope := range scopes {
		if len(scope.FailoverServers) != len(f.Peers) {
			return fmt.Errorf("dhcp scope %s: failoverServers must contain all three peers", scope.Name)
		}
		byNode := map[string]netip.Addr{}
		for _, server := range scope.FailoverServers {
			byNode[server.NodeID] = server.Address
		}
		for _, peer := range f.Peers {
			if !byNode[peer.ID].IsValid() {
				return fmt.Errorf("dhcp scope %s: missing failover server %s", scope.Name, peer.ID)
			}
		}
		if byNode[f.NodeID] != scope.ServerIP {
			return fmt.Errorf("dhcp scope %s: serverIP must match failoverServers for node %s", scope.Name, f.NodeID)
		}
	}
	return nil
}
