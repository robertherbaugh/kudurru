package memberidentity

import (
	"crypto/tls"
	"crypto/x509"
	"time"
)

// EnrollmentServerName selects the isolated invitation route on the original
// primary. It is not a member credential or permission to use member routes.
func (t Trust) EnrollmentServerName() string {
	if t.root == nil {
		return ""
	}
	return t.member + ".join.kudurru.invalid"
}

// EnrollmentClientTLS verifies the independently delivered invitation CA using
// normal chain and hostname verification before sending any bearer bytes. The
// extra check retains the original member identity too. No discovery, shared-CA
// fallback, client certificate or session cache is provided.
func (t Trust) EnrollmentClientTLS() (*tls.Config, error) {
	if !t.ValidAt(time.Now()) {
		return nil, ErrIdentity
	}
	roots := x509.NewCertPool()
	roots.AddCert(t.root)
	return &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: t.EnrollmentServerName(),
		VerifyConnection: func(state tls.ConnectionState) error {
			// Go invokes this AFTER normal chain/hostname verification but
			// BEFORE HandshakeComplete. VerifyPeer is for completed transports.
			if !t.ValidAt(time.Now()) || len(state.PeerCertificates) == 0 || state.PeerCertificates[0].VerifyHostname(memberName(t.member)) != nil {
				return ErrIdentity
			}
			return nil
		},
	}, nil
}

// EnrollmentServerTLS authenticates this original primary to an unknown joining
// node. Dispatch MUST isolate this SNI from all member and legacy routes. Only a
// checked invitation and signed key-possession proposal may admit the joiner.
// Its separate leaf cannot authenticate a client to an enrolled member socket.
func (i *Identity) EnrollmentServerTLS() (*tls.Config, error) {
	if i == nil || !i.trust.ValidAt(time.Now()) {
		return nil, ErrIdentity
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13, SessionTicketsDisabled: true,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) { return i.certificateForEnrollment(true) },
	}, nil
}
