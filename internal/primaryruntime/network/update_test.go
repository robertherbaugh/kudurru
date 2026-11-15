package network_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/miekg/dns"
	public "github.com/robertherbaugh/kudurru/api/v1alpha1"
	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	pr "github.com/robertherbaugh/kudurru/internal/primaryruntime"
	network "github.com/robertherbaugh/kudurru/internal/primaryruntime/network"
	"github.com/robertherbaugh/kudurru/internal/servingbuild"
	"github.com/robertherbaugh/kudurru/internal/servingdns"
	"github.com/robertherbaugh/kudurru/internal/stages/authoritative"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/zone"
)

type requestLeaseKey struct{}

func nativeListeners(t *testing.T, f *fixture, capture network.CaptureUpdate, observe func(pr.UpdateOutcome), encrypted ...dnsserver.Protocol) (*dnsserver.Registry, *tls.Config) {
	t.Helper()
	auth := authoritative.New(nil)
	var err error
	auth.Update, err = network.NewUpdate(network.UpdateOptions{Runtime: f.r, Capture: capture, Observe: observe})
	check(t, err)
	if len(encrypted) != 0 {
		// The encrypted test requires the real transport gate to refuse even
		// before a secondary receiver/refresh capability could be selected.
		auth.Notify = servingbuild.CapturedNotify(nil, time.Now)
	}
	chain := pipeline.MustNew(&ingress.Stage{}, auth)
	chain.SetCapture(func(req *pipeline.Request) (pipeline.ServingLease, error) {
		lease, err := f.pub.CaptureHTTP(req.Ctx)
		if err == nil {
			req.Ctx = context.WithValue(req.Ctx, requestLeaseKey{}, lease)
		}
		return servingdns.Wrap(lease), err
	})
	logger := slog.New(slog.DiscardHandler)
	registry, err := dnsserver.NewRegistry(dnsserver.Options{Handler: &dnsserver.Handler{Chain: chain, Logger: logger}, Logger: logger, TSIG: f.keys.Capture().Keys(), Sockets: 1})
	check(t, err)
	cert, key, pool := certificate(t)
	protocols := append([]dnsserver.Protocol{dnsserver.ProtoUDP, dnsserver.ProtoTCP, dnsserver.ProtoDoT}, encrypted...)
	for _, proto := range protocols {
		check(t, registry.Add(dnsserver.Spec{Protocol: proto, Address: "127.0.0.1:0", CertFile: cert, KeyFile: key}))
	}
	check(t, registry.Bind())
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- registry.Run(ctx) }()
	t.Cleanup(func() { cancel(); check(t, <-done) })
	for deadline := time.Now().Add(5 * time.Second); ; {
		if ready, _ := registry.Ready(); ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("native listeners not ready")
		}
		time.Sleep(time.Millisecond)
	}
	return registry, &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS12}
}

func certificate(t *testing.T) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	check(t, err)
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	check(t, err)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	check(t, err)
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	check(t, os.WriteFile(certFile, cert, 0600))
	check(t, os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0600))
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(cert) {
		t.Fatal("test certificate pool")
	}
	return certFile, keyFile, pool
}

func update(t *testing.T, apex, record string, signed bool) *dns.Msg {
	t.Helper()
	m := new(dns.Msg)
	m.SetUpdate(apex)
	if record != "" {
		rr, err := dns.NewRR(record)
		check(t, err)
		m.Insert([]dns.RR{rr})
	}
	if signed {
		m.SetTsig("key.", dns.HmacSHA256, 300, time.Now().Unix())
	}
	return m
}

func exchange(t *testing.T, _ *fixture, registry *dnsserver.Registry, tlsConfig *tls.Config, proto dnsserver.Protocol, m *dns.Msg) *dns.Msg {
	t.Helper()
	client := &dns.Client{Net: string(proto), Timeout: 4 * time.Second, TsigProvider: dnsserver.TSIGProvider(ring(t, "test-original-secret"))}
	if proto == dnsserver.ProtoDoT {
		client.Net = "tcp-tls"
		client.TLSConfig = tlsConfig
	}
	response, _, err := client.Exchange(m, registry.Addr(proto).String())
	// miekg stripTsig reports ErrAuth for ANY NOTAUTH rcode before checking
	// the MAC. The native denial response is still required and inspected.
	if !errors.Is(err, dns.ErrAuth) || response == nil || response.Rcode != dns.RcodeNotAuth {
		check(t, err)
	}
	if response == nil {
		t.Fatal("missing native response")
	}
	return response
}

func TestNativeSignedUpdatePublishesExactWholeBundleAndContainsDiagnosticPanic(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	registry, tlsConfig := nativeListeners(t, f, f.issuer.capture, func(pr.UpdateOutcome) { panic("test diagnostic fault") })
	for i, proto := range []dnsserver.Protocol{dnsserver.ProtoUDP, dnsserver.ProtoTCP, dnsserver.ProtoDoT} {
		t.Run(string(proto), func(t *testing.T) {
			name := fmt.Sprintf("host%d.example.", i)
			m := update(t, "example.", name+" 60 IN A 192.0.2.1", true)
			response := exchange(t, f, registry, tlsConfig, proto, m)
			if response.Rcode != dns.RcodeSuccess || response.IsTsig() == nil {
				t.Fatal("signed published outcome", response)
			}
			v := f.view()
			lease, err := f.pub.CaptureHTTP(f.ctx())
			check(t, err)
			defer lease.Release()
			value, _ := lease.Bundle().Component("runtime")
			owner := value.(*servingbuild.RuntimeOwner)
			member, ok := owner.Primary().DurableMember(f.l.Spec.Zone)
			if !ok || member.Version.CommitSequence != v.Head.Spec.CommitSequence || member.Version.RootDigest != v.Head.Spec.RootDigest {
				t.Fatal("wire success preceded exact complete publication")
			}
			if owner.Zones().Zones(time.Now()).Match(name).Lookup(name, dns.TypeA).Kind != zone.Success {
				t.Fatal("committed data missing from complete zone owner")
			}
		})
	}
}

func TestNativeDurablePublicationFailureIsServfailAndSameProofRecoversWithoutSemantics(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	results := make(chan [2]pr.UpdateOutcome, 1)
	capture := func(req *pipeline.Request, apex string) (pr.VerifiedUpdate, error) {
		command, err := f.issuer.capture(req, apex)
		if err == nil {
			f.publicationTimeout.Store(int64(250 * time.Millisecond))
			f.blocked.Store(true)
			f.pub.Wake()
		}
		return command, err
	}
	observe := func(first pr.UpdateOutcome) {
		before := f.view()
		f.publicationTimeout.Store(0)
		f.blocked.Store(false)
		f.pub.Wake()
		p := f.issuer.last
		again := f.r.Apply(p.req.Ctx, pr.VerifiedUpdate{Zone: f.l.Spec.Zone, Wire: p.wire, Proof: p})
		after := f.view()
		if !reflect.DeepEqual(before.Head.Spec, after.Head.Spec) {
			t.Error("same-proof recovery repeated semantics")
		}
		results <- [2]pr.UpdateOutcome{first, again}
	}
	registry, tlsConfig := nativeListeners(t, f, capture, observe)
	response := exchange(t, f, registry, tlsConfig, dnsserver.ProtoTCP, update(t, "example.", "dynamic.example. 60 IN A 192.0.2.9", true))
	out := <-results
	if response.Rcode != dns.RcodeServerFailure || response.IsTsig() == nil || !out[0].Committed || out[0].Published {
		t.Fatal("unpublished durable mutation leaked wire success", response, out[0])
	}
	if out[1].Rcode != dns.RcodeSuccess || !out[1].Published || !out[1].Deduplicated || len(f.view().Aggregate.Receipts) != 1 {
		t.Fatal("retained original proof recovery", out[1])
	}
}

func TestNativeUpdateExactApexAndUnsignedDenial(t *testing.T) {
	f := newFixture(t, public.ZonePolicy{})
	registry, tlsConfig := nativeListeners(t, f, f.issuer.capture, nil)
	before := f.view()
	for _, tc := range []struct {
		apex, rr string
		signed   bool
		code     int
	}{
		{"child.example.", "x.child.example. 60 IN A 192.0.2.1", true, dns.RcodeNotAuth},
		{"example.", "x.example. 60 IN A 192.0.2.1", false, dns.RcodeServerFailure},
		{"example.", "x.other. 60 IN A 192.0.2.1", true, dns.RcodeNotZone},
	} {
		response := exchange(t, f, registry, tlsConfig, dnsserver.ProtoTCP, update(t, tc.apex, tc.rr, tc.signed))
		if response.Rcode != tc.code {
			t.Fatal(response)
		}
	}
	if !reflect.DeepEqual(before, f.view()) {
		t.Fatal("denied native request mutated durable head")
	}
}
