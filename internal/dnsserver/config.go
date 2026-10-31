package dnsserver

import (
	"github.com/robertherbaugh/kudurru/internal/config"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
)

// LimitsFromConfig applies the native DNS limits to every listener in a
// registry. Zero counts remain unlimited and zero durations take the registry
// defaults. Configuration validation rejects negative values before startup.
func LimitsFromConfig(l config.Listeners) Limits {
	return Limits{
		MaxTCPQueries:   l.Limits.MaxTCPQueries,
		QUICMaxConns:    l.Limits.QUICMaxConns,
		IdleTimeout:     l.Limits.IdleTimeout.Std(),
		QUICIdleTimeout: l.Limits.QUICIdleTimeout.Std(),
	}
}

// SpecsFromConfig turns the listeners section of kudurru.yaml into registry
// specs: one Spec per protocol of every dns entry (udp and tcp when the
// list is empty) and one per dot, doh, doq and doh3 entry.
func SpecsFromConfig(l config.Listeners) []Spec {
	var specs []Spec
	for _, d := range l.DNS {
		protos := d.Protocols
		if len(protos) == 0 {
			protos = []string{string(ProtoUDP), string(ProtoTCP)}
		}
		for _, p := range protos {
			specs = append(specs, Spec{Protocol: Protocol(p), Address: d.Address})
		}
	}
	tlsSpec := func(p Protocol, address, path string, t config.TLSFiles) Spec {
		return Spec{Protocol: p, Address: address, Path: path, CertFile: t.CertFile, KeyFile: t.KeyFile}
	}
	for _, x := range l.DoT {
		s := tlsSpec(ProtoDoT, x.Address, "", x.TLS)
		s.ClientIDDomain = x.ClientIDDomain
		specs = append(specs, s)
	}
	for _, x := range l.DoH {
		s := tlsSpec(ProtoDoH, x.Address, x.Path, x.TLS)
		s.ClientIDs = x.ClientIDs
		specs = append(specs, s)
	}
	for _, x := range l.DoQ {
		s := tlsSpec(ProtoDoQ, x.Address, "", x.TLS)
		s.ClientIDDomain = x.ClientIDDomain
		specs = append(specs, s)
	}
	for _, x := range l.DoH3 {
		s := tlsSpec(ProtoDoH3, x.Address, x.Path, x.TLS)
		s.ClientIDs = x.ClientIDs
		specs = append(specs, s)
	}
	return specs
}

// PolicyFromConfig builds the PROXYv2 policy from
// listeners.proxyProtocolFrom and listeners.proxyProtocolSecretFile. An
// empty proxyProtocolFrom yields a disabled policy.
func PolicyFromConfig(l config.Listeners) (*proxyproto.Policy, error) {
	secret, err := proxyproto.LoadSecretFile(l.ProxyProtocolSecretFile)
	if err != nil {
		return nil, err
	}
	return proxyproto.NewPolicy(l.ProxyProtocolFrom, secret)
}
