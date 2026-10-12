//go:build spike

// Command spikecmd runs every kudurru DNS listener (udp, tcp, dot, doh, doq,
// doh3) on one host so operators can smoke-test them with dig, kdig, curl or
// dnsproxy, and so the dnsserver-quic spike can measure binary size. It is
// excluded from normal builds by the "spike" build tag and is not part of
// the kudurru binary; cmd/kudurru wires the same Registry.
//
//	go run -tags spike ./internal/dnsserver/spikecmd                  # static answers, generated cert
//	go run -tags spike ./internal/dnsserver/spikecmd -upstream 1.1.1.1:53
//	go run -tags spike,dnsproxy ./internal/dnsserver/spikecmd -selfcheck
//
// See docs/spikes/dnsserver-quic.md for the client commands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/dnsserver"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/internal/testcert"
	"github.com/robertherbaugh/kudurru/internal/dnsserver/proxyproto"
	"github.com/robertherbaugh/kudurru/internal/pipeline"
	"github.com/robertherbaugh/kudurru/internal/stages/ingress"
	"github.com/robertherbaugh/kudurru/internal/stages/upstream"
)

// selfcheck is set by selfcheck_dnsproxy.go when built with -tags dnsproxy.
var selfcheck func(addrs map[dnsserver.Protocol]string, certFile string) error

// staticAnswer answers every A query with 192.0.2.1 (TTL 60).
type staticAnswer struct{}

func (staticAnswer) Name() string { return "local" }

func (staticAnswer) Serve(req *pipeline.Request, next pipeline.Next) (*pipeline.Response, error) {
	q := req.Question()
	if q.Qtype != dns.TypeA {
		return next(req)
	}
	r := pipeline.Reply(req, dns.RcodeSuccess, pipeline.StatusLocalRecord)
	r.Msg.Answer = []dns.RR{&dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60}, A: net.IPv4(192, 0, 2, 1)}}
	return r, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "spikecmd:", err)
		os.Exit(1)
	}
}

func run() error {
	dnsAddr := flag.String("dns", "127.0.0.1:5300", "Do53 address (udp and tcp)")
	dotAddr := flag.String("dot", "127.0.0.1:8853", "DoT address (tcp)")
	dohAddr := flag.String("doh", "127.0.0.1:8443", "DoH address (tcp)")
	doqAddr := flag.String("doq", "127.0.0.1:8853", "DoQ address (udp)")
	doh3Addr := flag.String("doh3", "127.0.0.1:8443", "DoH3 address (udp)")
	certFile := flag.String("cert", "", "PEM certificate (generated for localhost/127.0.0.1 when empty)")
	keyFile := flag.String("key", "", "PEM key")
	up := flag.String("upstream", "", "forward to this resolver instead of answering A 192.0.2.1")
	proxyFrom := flag.String("proxy-from", "", "comma-separated PROXYv2 peers (exact hosts)")
	proxySecret := flag.String("proxy-secret-file", "", "PROXYv2 TLV 0xE0 secret file")
	check := flag.Bool("selfcheck", false, "query every listener through dnsproxy/upstream and exit (needs -tags dnsproxy)")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *certFile == "" {
		dir, err := os.MkdirTemp("", "kudurru-spike-")
		if err != nil {
			return err
		}
		pair, err := testcert.Write(dir, "")
		if err != nil {
			return err
		}
		*certFile, *keyFile = pair.CertFile, pair.KeyFile
		logger.Info("generated self-signed certificate", "cert", *certFile, "key", *keyFile)
	}

	stages := []pipeline.Stage{&ingress.Stage{NodeID: "spike", Version: "spike"}}
	if *up != "" {
		fwd, err := upstream.New("default", []string{*up}, 2*time.Second)
		if err != nil {
			return err
		}
		stages = append(stages, fwd)
	} else {
		stages = append(stages, staticAnswer{})
	}
	var from []string
	if *proxyFrom != "" {
		from = strings.Split(*proxyFrom, ",")
	}
	secret, err := proxyproto.LoadSecretFile(*proxySecret)
	if err != nil {
		return err
	}
	policy, err := proxyproto.NewPolicy(from, secret)
	if err != nil {
		return err
	}

	reg, err := dnsserver.NewRegistry(dnsserver.Options{
		Handler: &dnsserver.Handler{Chain: pipeline.MustNew(stages...), Logger: logger},
		Logger:  logger,
		Proxy:   policy,
	})
	if err != nil {
		return err
	}
	tls := func(p dnsserver.Protocol, a string) dnsserver.Spec {
		return dnsserver.Spec{Protocol: p, Address: a, CertFile: *certFile, KeyFile: *keyFile}
	}
	if err := reg.Add(
		dnsserver.Spec{Protocol: dnsserver.ProtoUDP, Address: *dnsAddr},
		dnsserver.Spec{Protocol: dnsserver.ProtoTCP, Address: *dnsAddr},
		tls(dnsserver.ProtoDoT, *dotAddr),
		tls(dnsserver.ProtoDoH, *dohAddr),
		tls(dnsserver.ProtoDoQ, *doqAddr),
		tls(dnsserver.ProtoDoH3, *doh3Addr),
	); err != nil {
		return err
	}
	if err := reg.Bind(); err != nil {
		return err
	}
	addrs := map[dnsserver.Protocol]string{}
	for _, p := range dnsserver.Protocols() {
		addrs[p] = reg.Addr(p).String()
		fmt.Printf("%-5s %s\n", p, addrs[p])
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *check {
		if selfcheck == nil {
			return fmt.Errorf("-selfcheck needs -tags spike,dnsproxy")
		}
		ctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- reg.Run(ctx) }()
		for ok, _ := reg.Ready(); !ok; ok, _ = reg.Ready() {
			time.Sleep(5 * time.Millisecond)
		}
		err := selfcheck(addrs, *certFile)
		cancel()
		if rerr := <-done; err == nil {
			err = rerr
		}
		return err
	}
	return reg.Run(ctx)
}
