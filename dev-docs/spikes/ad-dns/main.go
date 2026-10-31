// Command ad-dns is an isolated Kerberos/GSS-TSIG feasibility probe. It never
// accepts DNS changes and is not compiled into the kudurru product.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jcmturner/gokrb5/v8/keytab"
	"github.com/jcmturner/gokrb5/v8/types"
	"github.com/miekg/dns"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "ad-dns:", err)
		os.Exit(1)
	}
}

func run() error {
	address := flag.String("listen", "127.0.0.1:15353", "UDP and TCP listen address")
	ktPath := flag.String("keytab", "", "DNS service keytab file (never inline key material)")
	spn := flag.String("principal", "", "exact DNS service principal, DNS/host.example@REALM")
	zone := flag.String("zone", "probe.example.", "disposable lab zone used for SOA discovery")
	allow := flag.String("allow", "127.0.0.0/8,::1/128", "comma-separated lab client CIDRs")
	flag.Parse()
	principal, realm := types.ParseSPNString(*spn)
	if *ktPath == "" || realm == "" || len(principal.NameString) != 2 || principal.NameString[0] != "DNS" {
		return errors.New("-keytab and -principal DNS/hostname@REALM are required")
	}
	if _, ok := dns.IsDomainName(*zone); !ok || dns.CanonicalName(*zone) == "." {
		return errors.New("-zone must name a disposable non-root lab zone")
	}
	kt, err := keytab.Load(*ktPath)
	if err != nil {
		return errors.New("could not load service keytab")
	}
	p := &probe{acceptor: &acceptor{keytab: kt, principal: principal, realm: realm},
		logger: slog.Default(), contexts: make(map[string]*securityContext),
		zone: dns.CanonicalName(*zone), hostname: dns.CanonicalName(principal.NameString[1])}
	for _, value := range strings.Split(*allow, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(value))
		if err != nil {
			return fmt.Errorf("invalid client CIDR: %w", err)
		}
		p.allowed = append(p.allowed, prefix)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runServers(ctx, *address, p, func(addr string) {
		p.logger.Info("AD DNS probe ready; all updates will be refused", "address", addr)
	})
}

func runServers(ctx context.Context, address string, p *probe, ready func(string)) error {
	tcp, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer func() { _ = tcp.Close() }()
	udp, err := net.ListenPacket("udp", tcp.Addr().String())
	if err != nil {
		return err
	}
	defer func() { _ = udp.Close() }()
	started := make(chan struct{}, 2)
	servers := []*dns.Server{
		{Listener: tcp, Handler: p, TsigProvider: p, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second},
		{PacketConn: udp, Handler: p, TsigProvider: p, UDPSize: 65535},
	}
	done := make(chan error, 2)
	for _, server := range servers {
		server.MsgAcceptFunc = func(h dns.Header) dns.MsgAcceptAction {
			if h.Bits&0x8000 != 0 {
				return dns.MsgIgnore
			}
			if int(h.Bits>>11)&15 == dns.OpcodeUpdate {
				if h.Qdcount != 1 || uint32(h.Ancount)+uint32(h.Nscount)+uint32(h.Arcount) > 1024 {
					return dns.MsgReject
				}
				return dns.MsgAccept
			}
			return dns.DefaultMsgAcceptFunc(h)
		}
		server.NotifyStartedFunc = func() { started <- struct{}{} }
		go func() { done <- server.ActivateAndServe() }()
	}
	for range servers {
		<-started
	}
	ready(tcp.Addr().String())
	var serveErr error
	finished := 0
	select {
	case <-ctx.Done():
	case serveErr = <-done:
		finished++
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, server := range servers {
		if err := server.ShutdownContext(shutdown); err != nil && serveErr == nil {
			serveErr = err
		}
	}
	for ; finished < len(servers); finished++ {
		if err := <-done; err != nil && serveErr == nil {
			serveErr = err
		}
	}
	return serveErr
}
