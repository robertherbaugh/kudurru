package dhcpha

import (
	"crypto/tls"
	"errors"

	"github.com/robertherbaugh/kudurru/internal/cluster/internalapi"
)

// TLSFromCredentials upgrades the DNS internal plane's credentials to mutual
// authentication. Its ordinary ServerTLS deliberately does not request client
// certificates, and ClientTLS deliberately does not send one. HA requires both.
// NewCredentials must have been given this voter's NodeID as NodeName.
func TLSFromCredentials(c *internalapi.Credentials) (*tls.Config, error) {
	if c == nil {
		return nil, errors.New("dhcp HA: credentials are required")
	}
	server, client := c.ServerTLS(), c.ClientTLS()
	if client.RootCAs == nil || server.GetCertificate == nil {
		return nil, errors.New("dhcp HA: credentials need explicit certificate roots")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    client.RootCAs, ClientCAs: client.RootCAs,
		ServerName:     client.ServerName,
		ClientAuth:     tls.RequireAndVerifyClientCert,
		GetCertificate: server.GetCertificate,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return server.GetCertificate(&tls.ClientHelloInfo{})
		},
	}, nil
}
