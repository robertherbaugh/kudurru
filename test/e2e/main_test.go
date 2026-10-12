// Package e2e runs `kudurru serve` processes on loopback: two nodes sharing
// one encryption key (the internal listener), and serve-level resilience
// (SIGTERM lameduck, restarts over existing databases, configuration and
// port failures). The tests need KUDURRU_E2E=1 (make e2e).
package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/miekg/dns"

	"github.com/robertherbaugh/kudurru/internal/testutil/kudurrutest"
)

var kudurruBin string

func TestMain(m *testing.M) { os.Exit(kudurrutest.MainBuild(m, &kudurruBin)) }

// exchange sends one question over UDP (TCP when tcp is set).
func exchange(addr, name string, qtype uint16, tcp bool) (*dns.Msg, error) {
	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(name), qtype)
	m.RecursionDesired = true
	c := &dns.Client{Timeout: 3 * time.Second}
	if tcp {
		c.Net = "tcp"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	r, _, err := c.ExchangeContext(ctx, m, addr)
	return r, err
}

// firstA returns the first A record of r, or "".
func firstA(r *dns.Msg) string {
	if r == nil {
		return ""
	}
	for _, rr := range r.Answer {
		if a, ok := rr.(*dns.A); ok {
			return a.A.String()
		}
	}
	return ""
}

// zoneObjects returns inline objects for a primary zone e2e.test. with
// one A record per name=ip pair.
func zoneObjects(records map[string]string) []any {
	var eps []any
	for name, ip := range records {
		eps = append(eps, map[string]any{"dnsName": name + ".e2e.test", "recordType": "A", "targets": []any{ip}})
	}
	return []any{
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSZone",
			"metadata": map[string]any{"name": "e2e-test"},
			"spec": map[string]any{
				"zoneName": "e2e.test.", "type": "primary",
				"soa":         map[string]any{"mname": "ns1.e2e.test.", "rname": "hostmaster.e2e.test."},
				"nameservers": []any{"ns1.e2e.test."},
			},
		},
		map[string]any{
			"apiVersion": "kudurru.io/v1alpha1", "kind": "DNSRecordSet",
			"metadata": map[string]any{"name": "hosts", "namespace": "default"},
			"spec":     map[string]any{"zoneRef": map[string]any{"name": "e2e-test"}, "endpoints": eps},
		},
	}
}
