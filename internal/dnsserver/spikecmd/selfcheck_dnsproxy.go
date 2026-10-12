//go:build spike && dnsproxy

package main

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/AdguardTeam/dnsproxy/upstream"
	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
)

func init() { selfcheck = dnsproxySelfcheck }

// dnsproxySelfcheck queries every listener through dnsproxy's upstream
// package (the client library kudurru's upstream stage will use), trusting
// only the listener certificate.
func dnsproxySelfcheck(addrs map[dnsserver.Protocol]string, certFile string) error {
	pem, err := os.ReadFile(certFile) //nolint:gosec // operator-supplied path
	if err != nil {
		return err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return errors.New("no certificate in " + certFile)
	}
	urls := map[dnsserver.Protocol]string{
		dnsserver.ProtoUDP:  "udp://" + addrs[dnsserver.ProtoUDP],
		dnsserver.ProtoTCP:  "tcp://" + addrs[dnsserver.ProtoTCP],
		dnsserver.ProtoDoT:  "tls://" + addrs[dnsserver.ProtoDoT],
		dnsserver.ProtoDoH:  "https://" + addrs[dnsserver.ProtoDoH] + dnsserver.DefaultDoHPath,
		dnsserver.ProtoDoQ:  "quic://" + addrs[dnsserver.ProtoDoQ],
		dnsserver.ProtoDoH3: "h3://" + addrs[dnsserver.ProtoDoH3] + dnsserver.DefaultDoHPath,
	}
	var failed int
	for _, p := range dnsserver.Protocols() {
		u, err := upstream.AddressToUpstream(urls[p], &upstream.Options{RootCAs: pool, Timeout: 3 * time.Second, Logger: slog.New(slog.DiscardHandler)})
		if err != nil {
			return err
		}
		q := new(dns.Msg)
		q.SetQuestion("example.com.", dns.TypeA)
		start := time.Now()
		resp, err := u.Exchange(context.Background(), q)
		el := time.Since(start)
		_ = u.Close()
		if err != nil {
			failed++
			fmt.Printf("FAIL %-5s %-45s %v\n", p, urls[p], err)
			continue
		}
		fmt.Printf("ok   %-5s %-45s %s %d answer(s) in %s: %v\n", p, urls[p], dns.RcodeToString[resp.Rcode], len(resp.Answer), el.Round(time.Microsecond), resp.Answer)
	}
	if failed > 0 {
		return fmt.Errorf("%d transport(s) failed", failed)
	}
	return nil
}
