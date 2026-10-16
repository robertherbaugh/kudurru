package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dhcp"
	"github.com/robertherbaugh/kudurru/internal/dhcpha"
)

func dhcpFailoverTLS(cfg config.DHCPFailover) (*tls.Config, error) {
	if cfg.CertFile == "" {
		key, previous, err := readEncryptionKeys()
		if err != nil {
			return nil, err
		}
		credentials, err := internalapi.NewCredentials(internalapi.CredentialOptions{Key: key, PreviousKey: previous, NodeName: cfg.NodeID})
		if err != nil {
			return nil, fmt.Errorf("DHCP failover credentials: provide the cluster encryption key or all three TLS files: %w", err)
		}
		return dhcpha.TLSFromCredentials(credentials)
	}
	certificate, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("DHCP failover certificate: %w", err)
	}
	pem, err := os.ReadFile(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("DHCP failover CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("DHCP failover CA file has no certificates")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{certificate}, RootCAs: roots, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert}, nil
}

func openDHCPFailover(cfg *config.Config, stores map[string]*dhcp.LeaseStore, service *dhcp.Service, logger *slog.Logger) (*dhcpha.Node, error) {
	f := cfg.DHCP.Failover
	tlsConfig, err := dhcpFailoverTLS(f)
	if err != nil {
		return nil, err
	}
	peers := make([]dhcpha.Peer, 0, len(f.Peers))
	for _, peer := range f.Peers {
		peers = append(peers, dhcpha.Peer{ID: peer.ID, Address: peer.Address})
	}
	node, err := dhcpha.Open(dhcpha.Config{NodeID: f.NodeID, ClusterID: f.ClusterID, ListenAddress: f.ListenAddress,
		DataDir: filepath.Join(cfg.Node.DataDir, "dhcp-raft"), Peers: peers, Bootstrap: f.Bootstrap, TLS: tlsConfig, Logger: logger}, stores)
	if err != nil {
		return nil, err
	}
	for name := range stores {
		if err := service.SetBackend(name, node.Backend(name)); err != nil {
			_ = node.Close()
			return nil, err
		}
	}
	service.SetReplicationStatus(func() dhcp.ReplicationStatus {
		s := node.Status()
		return dhcp.ReplicationStatus{Enabled: s.Enabled, NodeID: s.NodeID, ClusterID: s.ClusterID, Role: s.Role, LeaderID: s.LeaderID,
			LeaderAddress: s.LeaderAddress, Ready: s.Ready, Initialized: s.Initialized, AppliedIndex: s.AppliedIndex, CommitIndex: s.CommitIndex, LastContact: s.LastContact, Error: s.Error}
	})
	return node, nil
}
