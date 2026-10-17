// Package dhcpha provides durable, quorum-backed DHCPv4 ownership. DNS cluster
// membership and VIP election deliberately do not participate in this quorum.
package dhcpha

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/robertherbaugh/kudurru/internal/dhcp"
)

var (
	// ErrNotLeader means DHCP ownership belongs to another voter.
	ErrNotLeader = errors.New("dhcp HA: this node is not the leader")
	// ErrNoQuorum means current majority authority could not be established.
	ErrNoQuorum = errors.New("dhcp HA: quorum is unavailable")
	// ErrUnavailable means startup, storage failure, or shutdown prevents work.
	ErrUnavailable = errors.New("dhcp HA: replica is unavailable")
)

// Peer is one immutable voting member. Address is the authenticated Raft TCP
// endpoint, not the public dashboard API address.
type Peer struct {
	ID      string `json:"id"`
	Address string `json:"address"`
}

// Config describes a static three-voter cluster. Bootstrap must be explicitly
// enabled only on the lexicographically first peer, with empty Raft storage.
type Config struct {
	NodeID, ClusterID, ListenAddress, DataDir string
	Peers                                     []Peer
	Bootstrap                                 bool
	TLS                                       *tls.Config
	RequestTimeout                            time.Duration
	Logger                                    *slog.Logger
}

const (
	defaultRequestTimeout = 1500 * time.Millisecond
	maxCommandBytes       = 8 << 20
	maxSnapshotBytes      = 256 << 20
)

func (c *Config) validate() error {
	if c.NodeID == "" || c.ClusterID == "" || len(c.NodeID) > 128 || len(c.ClusterID) > 128 {
		return errors.New("dhcp HA: stable node ID and cluster ID are required (maximum 128 bytes)")
	}
	if c.DataDir == "" || c.ListenAddress == "" || len(c.Peers) != 3 {
		return errors.New("dhcp HA: data directory, listen address, and exactly three voters are required")
	}
	c.Peers = append([]Peer(nil), c.Peers...)
	sort.Slice(c.Peers, func(i, j int) bool { return c.Peers[i].ID < c.Peers[j].ID })
	ids, addrs := map[string]bool{}, map[string]bool{}
	found := false
	for _, p := range c.Peers {
		if p.ID == "" || len(p.ID) > 128 || ids[p.ID] || addrs[p.Address] {
			return errors.New("dhcp HA: voter IDs and addresses must be distinct and nonempty")
		}
		host, port, err := net.SplitHostPort(p.Address)
		if err != nil || host == "" || port == "" || port == "0" || strings.ContainsAny(host, " \t\n") {
			return fmt.Errorf("dhcp HA: invalid advertised voter address %q", p.Address)
		}
		if p.ID == c.NodeID {
			found = true
		}
		ids[p.ID], addrs[p.Address] = true, true
	}
	if !found {
		return errors.New("dhcp HA: node ID is absent from voter list")
	}
	if c.Bootstrap && c.NodeID != c.Peers[0].ID {
		return errors.New("dhcp HA: only the first voter ID may explicitly bootstrap")
	}
	if c.TLS == nil || c.TLS.InsecureSkipVerify || c.TLS.RootCAs == nil || c.TLS.ClientCAs == nil || c.TLS.ClientAuth != tls.RequireAndVerifyClientCert {
		return errors.New("dhcp HA: verified mutual TLS with explicit root and client CAs is required")
	}
	if len(c.TLS.Certificates) == 0 && (c.TLS.GetCertificate == nil || c.TLS.GetClientCertificate == nil) {
		return errors.New("dhcp HA: both server and client certificates are required")
	}
	if c.TLS.GetConfigForClient != nil {
		return errors.New("dhcp HA: per-client TLS configuration overrides are unsupported")
	}
	var cert *tls.Certificate
	if len(c.TLS.Certificates) > 0 {
		cert = &c.TLS.Certificates[0]
	} else {
		var err error
		cert, err = c.TLS.GetCertificate(&tls.ClientHelloInfo{})
		if err != nil {
			return fmt.Errorf("dhcp HA: read node certificate: %w", err)
		}
	}
	if cert == nil || len(cert.Certificate) == 0 {
		return errors.New("dhcp HA: missing node certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return err
	}
	if leaf.Subject.CommonName != c.NodeID {
		return errors.New("dhcp HA: certificate common name must match node ID")
	}
	c.TLS = c.TLS.Clone()
	c.TLS.MinVersion = tls.VersionTLS13
	c.TLS.NextProtos = []string{"kudurru-dhcp-ha/1"}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = defaultRequestTimeout
	}
	if c.RequestTimeout < 100*time.Millisecond || c.RequestTimeout > 5*time.Second {
		return errors.New("dhcp HA: request timeout must be between 100ms and 5s")
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return nil
}

// fingerprint covers consensus identity, the voter list, and shared immutable
// scope topology. API-owned pool values are authoritative in the replicated
// state and must not turn a lagging replica into a different cluster.
func fingerprint(c Config, stores map[string]*dhcp.LeaseStore) (string, error) {
	topology := map[string]map[string]json.RawMessage{}
	for name, s := range stores {
		cfg := s.Configuration().Config
		b, err := json.Marshal(cfg)
		if err != nil {
			return "", err
		}
		fields := map[string]json.RawMessage{}
		if err := json.Unmarshal(b, &fields); err != nil {
			return "", err
		}
		delete(fields, "Interface")
		delete(fields, "ServerIP")
		if cfg.Management == "api" {
			typ := reflect.TypeOf(dhcp.PoolOptions{})
			for i := 0; i < typ.NumField(); i++ {
				delete(fields, typ.Field(i).Name)
			}
			delete(fields, "Reservations")
		}
		topology[name] = fields
	}
	b, err := json.Marshal(struct {
		Cluster string
		Peers   []Peer
		Scopes  any
	}{c.ClusterID, c.Peers, topology})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}
