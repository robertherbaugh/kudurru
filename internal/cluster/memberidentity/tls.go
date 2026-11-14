package memberidentity

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"time"
)

// ClientTLS authenticates exactly the original enrolled peer CA before sending
// any application bytes and presents this node's own certificate. The returned
// configuration is immutable per captured exchange. Connection pools must also
// be scoped to that exact Trust; a successor enrollment cannot reuse an old pool.
func (i *Identity) ClientTLS(peer Trust) (*tls.Config, error) {
	if i == nil || i.trust.root == nil || peer.root == nil || i.trust.member == peer.member {
		return nil, ErrIdentity
	}
	roots := x509.NewCertPool()
	roots.AddCert(peer.root)
	return &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: memberName(peer.member),
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) { return i.certificate() },
	}, nil
}

// ServerTLS admits only certificates chained to the captured enrolled member
// CAs, at most 64. Request routing must additionally match the claimed UUID to
// its exact Trust using VerifyPeer and retain membership through final effects.
// This strict listener is for enrolled peers, not legacy shared-key status reads.
func (i *Identity) ServerTLS(peers []Trust) (*tls.Config, error) {
	if i == nil || i.trust.root == nil || len(peers) == 0 || len(peers) > 64 {
		return nil, ErrIdentity
	}
	roots := x509.NewCertPool()
	seen := make(map[string]bool, len(peers))
	for _, peer := range peers {
		if peer.root == nil || peer.member == i.trust.member || seen[peer.member] {
			return nil, ErrIdentity
		}
		seen[peer.member] = true
		roots.AddCert(peer.root)
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs: roots, SessionTicketsDisabled: true,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return i.certificate() },
	}, nil
}

// VerifyPeer matches the actual completed TLS connection to one original
// enrolled member and checks all original certificate lifetimes again. It does
// not look up current membership or grant domain/administrator permissions.
// Callers must pass the transport's actual connection state, never claimed data.
func (t Trust) VerifyPeer(state *tls.ConnectionState) error {
	_, err := t.peerExpiry(state, time.Now())
	return err
}

func (t Trust) peerExpiry(state *tls.ConnectionState, now time.Time) (time.Time, error) {
	if t.root == nil || state == nil || !state.HandshakeComplete || state.Version < tls.VersionTLS13 || len(state.PeerCertificates) == 0 {
		return time.Time{}, ErrIdentity
	}
	leaf := state.PeerCertificates[0]
	if leaf == nil || leaf.VerifyHostname(memberName(t.member)) != nil {
		return time.Time{}, ErrIdentity
	}
	for _, chain := range state.VerifiedChains {
		if len(chain) < 2 || chain[0] == nil || chain[len(chain)-1] == nil || !bytes.Equal(chain[0].Raw, leaf.Raw) || !bytes.Equal(chain[len(chain)-1].Raw, t.root.Raw) {
			continue
		}
		valid := true
		expires := leaf.NotAfter
		for _, cert := range chain {
			if cert == nil || now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
				valid = false
				break
			}
			if cert.NotAfter.Before(expires) {
				expires = cert.NotAfter
			}
		}
		if valid {
			return expires, nil
		}
	}
	return time.Time{}, ErrIdentity
}
