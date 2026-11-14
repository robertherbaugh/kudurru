package main

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"github.com/robertherbaugh/kudurru/internal/recursor"
	"github.com/robertherbaugh/kudurru/internal/trustanchors"
)

// TestLiveRootManagedBootstrap (F28-RFC-3, D398) runs the production managed
// acquisition against every built-in root server address: the wire plan
// (RD clear, EDNS 1232 with DO, TCP), the first-complete-response read, the
// strict bootstrap unpack (no authority records, at most the OPT record) and
// trustanchors.Bootstrap with each shipped IANA DS seed on its own. Every
// reachable endpoint must give a reply the strict parser accepts, and at
// least one shipped seed must authenticate the root DNSKEY set. The test
// logs which seeds authenticate today. It needs outbound DNS over TCP and
// runs only with KUDURRU_LIVE_DNS=1 (dev-docs/release-checklist.md).
func TestLiveRootManagedBootstrap(t *testing.T) {
	if testing.Short() || os.Getenv("KUDURRU_LIVE_DNS") == "" {
		t.Skip("set KUDURRU_LIVE_DNS=1 to bootstrap from the real root servers")
	}
	clock, err := trustanchors.NewSessionClock()
	if err != nil {
		t.Fatal(err)
	}
	reachable, authenticated := 0, map[string]int{}
	for offset, endpoint := range recursor.DefaultRootHints() {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		plan, err := captureAnchorWirePlan(ctx, []netip.AddrPort{endpoint}, "tcp", uint64(offset))
		if err != nil {
			cancel()
			t.Fatal("wire plan", endpoint, err)
		}
		raw, sample, err := acquireAnchorWire(plan, func() (trustanchors.Sample, error) { return clock.Sample(), nil })
		cancel()
		if errors.Is(err, errAnchorAcquisition) && endpoint.Addr().Is6() {
			// Hosts without IPv6 egress cannot reach the v6 addresses; an
			// incomplete transport is not a reply.
			t.Logf("%s: unreachable (no complete reply)", endpoint)
			continue
		}
		if err != nil {
			t.Errorf("%s: no complete acquisition: %v", endpoint, err)
			continue
		}
		reachable++
		message, err := unpackAnchorBootstrap(raw)
		if err != nil {
			t.Errorf("%s: the strict bootstrap parser refused a %d-byte reply: %v", endpoint, len(raw), err)
			continue
		}
		var tags, signers []string
		for _, rr := range message.Answer {
			switch v := rr.(type) {
			case *dns.DNSKEY:
				if v.Flags&dns.SEP != 0 {
					tags = append(tags, strconv.Itoa(int(v.KeyTag())))
				}
			case *dns.RRSIG:
				signers = append(signers, strconv.Itoa(int(v.KeyTag)))
			}
		}
		var ok []string
		for _, seed := range recursor.DefaultTrustAnchors {
			_, observation, err := trustanchors.Bootstrap(trustanchors.Seed{ClusterID: "live-root", PolicyID: "live-root-seed", DS: []string{seed}}, message, sample)
			if err != nil {
				continue
			}
			if err := observation.CheckLifetime(clock.Sample()); err != nil {
				t.Errorf("%s: authenticated observation already expired: %v", endpoint, err)
				continue
			}
			tag := strings.Fields(seed)[3]
			ok = append(ok, tag)
			authenticated[tag]++
		}
		if len(ok) == 0 {
			t.Errorf("%s: no shipped seed authenticates the root DNSKEY set (KSKs %v, signatures by %v)", endpoint, tags, signers)
			continue
		}
		t.Logf("%s: %d bytes, NSCOUNT 0, ARCOUNT %d, KSKs %v, signed by %v, authenticated by seeds %v", endpoint, len(raw), len(message.Extra), tags, signers, ok)
	}
	if reachable == 0 {
		t.Fatal("no root server endpoint was reachable over TCP")
	}
	for _, seed := range recursor.DefaultTrustAnchors {
		tag := strings.Fields(seed)[3]
		t.Logf("shipped seed %s authenticated the DNSKEY set on %d of %d reachable endpoints", tag, authenticated[tag], reachable)
	}
}
